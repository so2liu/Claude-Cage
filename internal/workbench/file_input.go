package workbench

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"al.essio.dev/pkg/shellescape"
	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/diagnostics"
	"github.com/sylvinus/agent-vm/internal/usererror"
)

// The broker belongs to Claude Desktop, not to the VM. An unopened desktop
// is an ordinary empty poll; a failed service must still surface as an error.
const desktopFilesPoll = `state=$(systemctl is-active cage-claude-desktop.service 2>/dev/null) || true
case "$state" in
  inactive|activating|deactivating|unknown)
    printf '%s\n' '{"available":false,"uploads":[],"downloads":[],"opens":[]}'
    exit 0 ;;
  active) ;;
  *) printf 'desktop file service state=%s\n' "$state" >&2; exit 1 ;;
esac
`

// Drain stderr even beyond the retained prefix; a noisy child must not block.
type fileControlStderr struct{ data []byte }

func (b *fileControlStderr) Write(p []byte) (int, error) {
	n := len(p)
	if remaining := 4096 - len(b.data); remaining > 0 {
		b.data = append(b.data, p[:min(len(p), remaining)]...)
	}
	return n, nil
}

var terminalFileID = regexp.MustCompile(`^[a-f0-9]{16}$`)

func validFileTargetID(target, id string) bool {
	if target == "terminal" {
		return terminalFileID.MatchString(id)
	}
	return browserFileID.MatchString(id)
}

// Desktop requests use the private session socket, carried over the existing
// SSH connection. Directory selections use an authorized project's guest path.
func (s *Server) guestFiles(ctx context.Context, target, method, path string, body io.Reader, name string) (map[string]any, error) {
	if s.macosGuest != nil && target == "desktop" {
		parsed, err := url.Parse(path)
		if err != nil {
			return nil, errors.New("虚拟 Mac 文件操作无效")
		}
		if method == "POST" && parsed.Path == "/upload" {
			return s.macOSUploadDesktopAttachment(ctx, name, parsed.Query().Get("id"), body)
		}
		var input macOSWindowRequest
		if body != nil {
			data, err := io.ReadAll(io.LimitReader(body, 8193))
			if err != nil || len(data) > 8192 || json.Unmarshal(data, &input) != nil {
				return nil, errors.New("虚拟 Mac 文件操作无效")
			}
		}
		switch {
		case method == "GET" && parsed.Path == "/requests":
			input.Action = "files-poll"
		case method == "POST" && (parsed.Path == "/start" || parsed.Path == "/finish" || parsed.Path == "/finish-image" || parsed.Path == "/cancel" || parsed.Path == "/directory" || parsed.Path == "/download-retry" || parsed.Path == "/download-ack" || parsed.Path == "/open-claim" || parsed.Path == "/open-complete"):
			input.Action = "files-" + strings.TrimPrefix(parsed.Path, "/")
		default:
			return nil, errors.New("虚拟 Mac 文件操作无效")
		}
		input.Files = nil
		return s.macOSDesktopFileCall(ctx, input)
	}
	if s.windowsGuest != nil && target == "terminal" {
		return s.windowsSaveAttachment(ctx, name, body)
	}
	if s.windowsGuest != nil && target == "desktop" {
		parsed, err := url.Parse(path)
		if err != nil {
			return nil, errors.New("Windows 文件操作无效")
		}
		if method == "POST" && parsed.Path == "/upload" {
			return s.windowsUploadAttachment(ctx, name, parsed.Query().Get("id"), body)
		}
		var action string
		switch {
		case method == "GET" && parsed.Path == "/requests":
			action = "files-poll"
		case method == "POST" && (parsed.Path == "/start" || parsed.Path == "/finish" || parsed.Path == "/finish-image" || parsed.Path == "/cancel" || parsed.Path == "/directory" || parsed.Path == "/download-retry" || parsed.Path == "/download-ack" || parsed.Path == "/open-claim" || parsed.Path == "/open-complete"):
			action = "files-" + strings.TrimPrefix(parsed.Path, "/")
		default:
			return nil, errors.New("Windows 原生文件选择框与桌面下载尚未接通")
		}
		input := map[string]any{}
		if body != nil {
			data, readErr := io.ReadAll(io.LimitReader(body, 65537))
			if readErr != nil || len(data) > 65536 || json.Unmarshal(data, &input) != nil {
				return nil, errors.New("Windows 文件操作无效")
			}
		}
		var result map[string]any
		err = s.windowsDesktopRequest(ctx, action, input, &result)
		return result, windowsDesktopFileFailure(err)
	}
	script := "sudo -H -u cage curl --silent --show-error --max-time 1800 --write-out '\\nCAGE_FILE_HTTP:%{http_code}' --unix-socket /run/cage-desktop/files.sock -X " + method
	if target == "terminal" {
		script = "sudo -H -u cage /usr/local/bin/cage-desktop-files --save-file " + shellescape.Quote(name)
		if s.macosGuest != nil {
			script = "exec " + shellescape.QuoteCommand([]string{macCLIInstall + "/bin/python3", "-I", "-c", cage.DesktopFileReceiver(), "--save-file", name})
		}
	} else {
		if body != nil {
			script += " --upload-file -"
		}
		script += " " + shellescape.Quote("http://localhost"+path)
	}
	var stderr fileControlStderr
	var data []byte
	var err, readErr error
	shared := false
	if target != "terminal" && method == "GET" && path == "/requests" {
		script = desktopFilesPoll + script
		// The workbench asks for pending requests every second or two.
		var reply guestSessionReply
		if reply, shared, err = s.guestShared(ctx, script); shared {
			data, stderr.data = reply.stdout, reply.stderr
			if err == nil && reply.code != 0 {
				err = fmt.Errorf("exit status %d", reply.code)
			}
		}
	}
	if !shared {
		command := "exec /bin/bash --noprofile --norc -c " + shellescape.Quote(script)
		cmd := s.sshCommand(ctx, "-T", s.sshTarget(), "--", command)
		cmd.Stderr = &stderr
		cmd.Stdin = body
		pipe, pipeErr := cmd.StdoutPipe()
		if pipeErr != nil {
			return nil, pipeErr
		}
		if err = cmd.Start(); err != nil {
			return nil, usererror.Wrap("连接文件传递服务", err, "请确认 Cage 安装完整，环境已经启动；持续失败时在设置中导出诊断包。")
		}
		data, readErr = io.ReadAll(io.LimitReader(pipe, 65537+64))
		if readErr != nil || len(data) > 65536+64 {
			_ = cmd.Process.Kill()
		}
		err = cmd.Wait()
	}
	httpStatus := 0
	if at := bytes.LastIndex(data, []byte("\nCAGE_FILE_HTTP:")); at >= 0 && target != "terminal" {
		httpStatus, _ = strconv.Atoi(strings.TrimSpace(string(data[at+len("\nCAGE_FILE_HTTP:"):])))
		data = data[:at]
	}
	var result map[string]any
	decodeErr := json.Unmarshal(data, &result)
	if err != nil || readErr != nil || len(data) > 65536 || decodeErr != nil || result == nil || httpStatus >= 400 {
		// No response body, filename, request ID or query string enters the log.
		route := strings.SplitN(path, "?", 2)[0]
		_ = diagnostics.New(s.profile).Record("error", "desktop-files", fmt.Sprintf("target=%s method=%s route=%s exit=%v read=%v invalid_json=%t oversized=%t context=%v stderr=%s", target, method, route, err, readErr, decodeErr != nil || result == nil, len(data) > 65536, ctx.Err(), stderr.data))
		switch {
		case ctx.Err() != nil:
			return nil, usererror.Wrap("传递文件", ctx.Err(), "请确认环境仍在运行；恢复连接后重新选择文件，并核对目标是否已收到文件。")
		case httpStatus >= 400:
			return nil, fileServiceFailure(httpStatus, data)
		case err != nil:
			return nil, executionFailure("连接文件传递服务", err, string(stderr.data))
		case readErr != nil:
			return nil, usererror.Wrap("读取文件传递结果", readErr, "结果没有完整返回，请恢复连接后核对文件是否已收到。")
		case len(data) > 65536:
			return nil, errors.New("读取文件传递结果失败：服务返回的数据超过 64 KiB。请分批处理文件；持续出现时导出诊断包。")
		default:
			return nil, errors.New("读取文件传递结果失败：服务没有返回有效的文件记录。请重新打开接收文件的应用；旧环境请先更新环境组件。")
		}
	}
	return result, nil
}

func (s *Server) fileTargetReady(target, id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ready {
		return false
	}
	if target == "terminal" {
		terminal := s.terminals.get(id)
		if terminal == nil {
			return false
		}
		terminal.mu.Lock()
		defer terminal.mu.Unlock()
		return !terminal.closed
	}
	return target == "browser" || target == "desktop"
}

func (s *Server) fileInputAction(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Target    string  `json:"target"`
		ID        string  `json:"id"`
		Kind      string  `json:"kind"`
		X         float64 `json:"x"`
		Y         float64 `json:"y"`
		Workspace string  `json:"workspace"`
		Window    uint32  `json:"window,omitempty"`
		PID       int32   `json:"pid,omitempty"`
	}
	if !readJSON(w, r, &input) {
		return
	}
	action := r.PathValue("action")
	if action != "start" && action != "finish" && action != "cancel" && action != "directory" && action != "download-retry" {
		http.NotFound(w, r)
		return
	}
	if !(action == "start" && (input.Target == "browser" && input.ID != "" && len(input.ID) <= 256 || input.Target == "desktop")) && !validFileTargetID(input.Target, input.ID) {
		http.Error(w, "文件目标无效", 400)
		return
	}
	if !s.fileTargetReady(input.Target, input.ID) {
		http.Error(w, "文件目标已关闭或环境尚未就绪", 409)
		return
	}
	if action == "start" && input.Kind != "drop" && input.Kind != "paste" {
		http.Error(w, "文件传递方式无效", 400)
		return
	}
	if (action == "directory" || action == "download-retry") && input.Target != "desktop" {
		http.Error(w, "文件夹目标无效", 400)
		return
	}
	if action == "directory" {
		if !s.op.TryLock() {
			http.Error(w, "另一个操作仍在进行，请稍候", 409)
			return
		}
		defer s.op.Unlock()
		if !s.fileTargetReady("desktop", input.ID) || s.busy() {
			http.Error(w, "环境尚未就绪或正在处理其他操作，请稍后重试", 409)
			return
		}
	}
	if action == "download-retry" && (s.macosGuest != nil || s.windowsGuest != nil) {
		if handled, err := s.retryNativeDownload(input.ID); handled {
			if err != nil {
				requestError(w, err)
			} else {
				jsonReply(w, map[string]bool{"ok": true})
			}
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	body, _ := json.Marshal(input)
	if action == "directory" {
		if input.Workspace == "" {
			http.Error(w, "请选择已打开的项目", 400)
			return
		}
		workspace, err := s.connectProject(input.Workspace, false)
		if err != nil {
			requestError(w, err)
			return
		}
		body, _ = json.Marshal(map[string]string{"id": input.ID, "path": guestPath(workspace)})
		if s.macosGuest != nil {
			body, _ = json.Marshal(map[string]string{"id": input.ID, "path": macOSProjectPath(s.macosGuest.User, workspace)})
		} else if s.windowsGuest != nil {
			project, err := s.windowsProjectForHost(workspace)
			if err != nil {
				requestError(w, err)
				return
			}
			body, _ = json.Marshal(map[string]string{"id": input.ID, "project_id": project.ID, "grant": project.Grant})
		}
	}
	if input.Target == "terminal" {
		jsonReply(w, map[string]any{"ok": true, "id": input.ID})
		return
	}
	if input.Target == "browser" {
		s.mu.Lock()
		token := s.credentials.Token
		s.mu.Unlock()
		data, err := s.guestAPI(ctx, token, "POST", "/upload-"+action, body)
		if err != nil {
			requestError(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(data)
		return
	}
	result, err := s.guestFiles(ctx, "desktop", "POST", "/"+action, bytes.NewReader(body), "")
	if err != nil {
		requestError(w, err)
		return
	}
	jsonReply(w, result)
}

func (s *Server) desktopFileRequests(w http.ResponseWriter, r *http.Request) {
	if !s.fileTargetReady("desktop", "") {
		http.Error(w, "环境尚未就绪", 409)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	result, err := s.guestFiles(ctx, "desktop", "GET", "/requests", nil, "")
	if err != nil {
		// Service/transport failures are not invalid user input. guestFiles has
		// already recorded the bounded diagnostic, including the real exit code.
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	if err = s.rememberDesktopDownloads(result); err != nil {
		requestError(w, err)
		return
	}
	s.acknowledgeDesktopDownloads(ctx, result)
	jsonReply(w, result)
}

func (s *Server) acknowledgeDesktopDownloads(ctx context.Context, result map[string]any) {
	if result["downloadAck"] != true || (s.macosGuest == nil && s.windowsGuest == nil) {
		return
	}
	var ids []string
	items, _ := result["downloads"].([]any)
	for _, value := range items {
		item, ok := value.(map[string]any)
		if !ok || item["state"] != "saved" || item["acknowledged"] == true {
			continue
		}
		id, _ := item["id"].(string)
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return
	}
	// rememberDesktopDownloads has resolved every saved state from its durable
	// host receipt. A failed acknowledgment keeps the guest copy for the next poll.
	body, _ := json.Marshal(map[string]any{"ids": ids})
	if _, err := s.guestFiles(ctx, "desktop", "POST", "/download-ack", bytes.NewReader(body), ""); err != nil {
		diagnostics.Log("warn", "desktop-files", "主机文件已保存，客机保留副本并等待下次确认："+err.Error())
	}
}

// Register completed exports before the UI can ask a native bridge to save them.
// Guest paths are never accepted as arbitrary host paths.
func (s *Server) rememberDesktopDownloads(result map[string]any) error {
	items, _ := result["downloads"].([]any)
	visible := make(map[string]bool, len(items))
	for _, value := range items {
		item, ok := value.(map[string]any)
		if !ok {
			return errors.New("Desktop 下载记录无效")
		}
		id, _ := item["id"].(string)
		name, _ := item["name"].(string)
		if !browserFileID.MatchString(id) || !browserFileName(name) {
			return errors.New("Desktop 下载记录无效")
		}
		visible[id] = true
		item["target"] = "desktop"
		if s.macosGuest != nil || s.windowsGuest != nil {
			data, err := json.Marshal(item)
			if err != nil {
				return err
			}
			var native browserDownloadInfo
			if err = json.Unmarshal(data, &native); err != nil {
				return err
			}
			converted := s.nativeDownloadForHost(native)
			item["name"], item["state"], item["path"] = converted.Name, converted.State, converted.Path
			item["retryable"], item["error"] = converted.Retryable, converted.Error
			continue
		}
		if item["state"] != "saved" {
			delete(item, "path")
			continue
		}
		root := s.environmentDir()
		if item["path"] != strings.TrimRight(guestPath(root), "/")+"/Downloads/"+name {
			return errors.New("Desktop 下载保存位置无效")
		}
		path := filepath.Join(root, "Downloads", name)
		if err := rememberDownload(s.profile, SavedDownload{ID: id, Name: name, Path: path}, true); err != nil {
			return err
		}
		item["path"] = path
	}
	if s.macosGuest != nil || s.windowsGuest != nil {
		s.discardMissingNativeDownloads("desktop", visible)
	}
	return nil
}

func (s *Server) fileInputUpload(w http.ResponseWriter, r *http.Request) {
	target, id, name := r.PathValue("target"), r.PathValue("id"), r.URL.Query().Get("name")
	if !validFileTargetID(target, id) || !browserFileName(name) {
		http.Error(w, "文件名或上传目标无效", 400)
		return
	}
	if !s.fileTargetReady(target, id) {
		http.Error(w, "文件目标已关闭或环境尚未就绪", 409)
		return
	}
	if target == "browser" {
		s.browserUpload(w, r)
		return
	}
	if r.ContentLength > browserFileLimit {
		http.Error(w, "每个文件不能超过 2 GB", 413)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()
	r.Body = http.MaxBytesReader(w, r.Body, browserFileLimit)
	result, err := s.guestFiles(ctx, target, "POST", "/upload?"+url.Values{"id": {id}, "name": {name}}.Encode(), r.Body, name)
	if err != nil {
		requestError(w, err)
		return
	}
	if target == "terminal" {
		path, _ := result["path"].(string)
		valid := strings.HasPrefix(path, "/home/cage/.local/share/cage/attachments/files/") && !strings.ContainsAny(path, "\x00\r\n")
		if s.windowsGuest != nil {
			valid = validWindowsAttachmentPath(path, name)
		}
		if s.macosGuest != nil {
			valid = s.validMacOSAttachmentPath(path, name)
		}
		if !valid {
			http.Error(w, "文件保存路径无效", 502)
			return
		}
	}
	jsonReply(w, result)
}
