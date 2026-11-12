package workbench

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"al.essio.dev/pkg/shellescape"
	"github.com/google/uuid"
	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/clipboardfiles"
	"github.com/sylvinus/agent-vm/internal/guestos"
)

const clipboardWireLimit = 30 << 20

// 保留当前后端配套的客机脚本，使已有 Linux 客机也能建立剪贴板连接。
// 将脚本压缩为单个参数，避免完整脚本经过两层转义后超出 Windows 进程命令行上限。
var linuxClipboardCommand = sync.OnceValues(func() (string, error) {
	var compressed bytes.Buffer
	writer := zlib.NewWriter(&compressed)
	if _, err := io.WriteString(writer, cage.SharedClipboardHelper); err != nil {
		return "", err
	}
	if err := writer.Close(); err != nil {
		return "", err
	}
	// 移除载荷参数后，辅助脚本仍收到原有操作参数，stdin 也仍用于剪贴板协议。
	const bootstrap = `import base64,sys,zlib; exec(compile(zlib.decompress(base64.b64decode(sys.argv.pop(1))), '<cage-shared-clipboard>', 'exec'))`
	return "exec sudo -n /usr/bin/python3 -c " + shellescape.Quote(bootstrap) + " " +
		base64.StdEncoding.EncodeToString(compressed.Bytes()), nil
})

type clipboardPayload struct {
	Hello   *clipboardHello `json:"hello,omitempty"`
	Text    *string         `json:"text,omitempty"`
	PNG     string          `json:"png,omitempty"`
	Files   json.RawMessage `json:"files,omitempty"`
	Error   string          `json:"error,omitempty"`
	Source  string          `json:"source,omitempty"`
	Serial  int             `json:"serial,omitempty"`
	Ack     int             `json:"ack,omitempty"`
	Release []string        `json:"release,omitempty"`
}

type clipboardHello struct {
	Version     int      `json:"version"`
	OS          string   `json:"os"`
	Session     string   `json:"session"`
	Formats     []string `json:"formats"`
	Available   bool     `json:"available"`
	FileRelease bool     `json:"file_release,omitempty"`
}

// The profile chooses the transport. A guest file selection can never choose
// an executable, account, remote command or host path.
func (s *Server) clipboardCommand(ctx context.Context, operation string, selection clipboardFileSelection) (*exec.Cmd, string, error) {
	win, err := guestos.LoadWindowsProfile(s.profile)
	if err != nil {
		return nil, "", err
	}
	if win != nil {
		if s.macosGuest != nil {
			return nil, "", errors.New("客机绑定冲突")
		}
		args := []string{}
		if operation == "export" {
			args = append(args, "--paths-stdin")
		}
		cmd, err := guestos.WindowsRelayCommand(ctx, s.profile, operation, args...)
		if err == nil && operation == "export" {
			data, _ := json.Marshal(selection.Paths)
			cmd.Stdin = bytes.NewReader(data)
		}
		return cmd, "guest", err
	}
	if s.macosGuest != nil {
		if operation != "stream" && operation != "import" && operation != "export" && operation != "upload" && operation != "request" {
			return nil, "", errors.New("剪贴板操作无效")
		}
		if operation == "export" && selection.User != "guest" {
			return nil, "", errors.New("剪贴板文件来源无效")
		}
		command := "exec '/Applications/Cage Guest Clipboard.app/Contents/MacOS/CageGuestClipboard' --client " + operation
		cmd := s.sshCommand(ctx, "-T", s.sshTarget(), "--", command)
		if operation == "export" {
			data, _ := json.Marshal(selection.Paths)
			cmd.Stdin = bytes.NewReader(data)
		}
		return cmd, "guest", nil
	}
	command, err := linuxClipboardCommand()
	if err != nil {
		return nil, "", err
	}
	switch operation {
	case "stream":
	case "import":
		s.mu.Lock()
		session := s.clipboardGuestSession
		s.mu.Unlock()
		command += " --import --session " + shellescape.Quote(session)
	case "export":
		if selection.User != "cage" && selection.User != "cage-browser" {
			return nil, "", errors.New("剪贴板文件来源无效")
		}
		command += " --export-user " + selection.User + " --paths-stdin"
	default:
		return nil, "", errors.New("剪贴板操作无效")
	}
	cmd := s.sshCommand(ctx, "-T", s.sshTarget(), "--", command)
	if operation == "export" {
		data, _ := json.Marshal(selection.Paths)
		cmd.Stdin = bytes.NewReader(data)
	}
	return cmd, "cage", nil
}

type clipboardFileSelection struct {
	Paths []string
	User  string
	lease *clipboardFileLease
}

type clipboardFileLease struct {
	readers int
	retired bool
	release func()
}

func (s *Server) registerClipboardFiles(paths []string, user string) string {
	s.mu.Lock()
	if s.clipboardFiles == nil {
		s.clipboardFiles = map[string]clipboardFileSelection{}
	}
	for id, previous := range s.clipboardFiles {
		if previous.User == user && slices.Equal(previous.Paths, paths) {
			s.mu.Unlock()
			return id
		}
	}
	// These are transient selection references, never file contents or history.
	var releases []func()
	if len(s.clipboardFiles) >= 100 {
		releases = s.retireClipboardFilesLocked()
	}
	id := uuid.NewString()
	ownedPaths := append([]string(nil), paths...)
	lease := &clipboardFileLease{}
	if release := s.clipboardRelease; release != nil {
		lease.release = func() { release(ownedPaths) }
	}
	s.clipboardFiles[id] = clipboardFileSelection{Paths: ownedPaths, User: user, lease: lease}
	s.mu.Unlock()
	for _, release := range releases {
		release()
	}
	return id
}

func (s *Server) retireClipboardFilesLocked() []func() {
	var releases []func()
	for id, selection := range s.clipboardFiles {
		delete(s.clipboardFiles, id)
		if selection.lease != nil {
			selection.lease.retired = true
			if selection.lease.readers == 0 && selection.lease.release != nil {
				releases = append(releases, selection.lease.release)
			}
		}
	}
	return releases
}

func (s *Server) clearClipboardFiles() {
	s.mu.Lock()
	releases := s.retireClipboardFilesLocked()
	s.mu.Unlock()
	for _, release := range releases {
		release()
	}
}

func (s *Server) releaseClipboardFiles(id string) {
	s.mu.Lock()
	selection := s.clipboardFiles[id]
	delete(s.clipboardFiles, id)
	var release func()
	if selection.lease != nil {
		selection.lease.retired = true
		if selection.lease.readers == 0 {
			release = selection.lease.release
		}
	}
	s.mu.Unlock()
	if release != nil {
		release()
	}
}

func (s *Server) acquireClipboardFiles(id string) (clipboardFileSelection, func()) {
	s.mu.Lock()
	selection := s.clipboardFiles[id]
	if selection.lease != nil {
		selection.lease.readers++
	}
	s.mu.Unlock()
	return selection, func() {
		var release func()
		s.mu.Lock()
		if selection.lease != nil {
			selection.lease.readers--
			if selection.lease.retired && selection.lease.readers == 0 {
				release = selection.lease.release
			}
		}
		s.mu.Unlock()
		if release != nil {
			release()
		}
	}
}

func (s *Server) clipboardFileSelection(id string) clipboardFileSelection {
	s.mu.Lock()
	defer s.mu.Unlock()
	selection := s.clipboardFiles[id]
	selection.Paths = append([]string(nil), selection.Paths...)
	return selection
}

func (s *Server) sharedClipboard(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	available := s.ready && !s.closing && s.clipboardCancel == nil
	ctx, cancel := context.WithCancel(r.Context())
	if available {
		s.clipboardCancel = cancel
	}
	s.mu.Unlock()
	if !available {
		cancel()
		http.Error(w, "剪贴板连接暂不可用", http.StatusConflict)
		return
	}
	defer func() { cancel(); s.mu.Lock(); s.clipboardCancel = nil; s.clipboardRelease = nil; s.mu.Unlock() }()
	// 以当前后端配套的辅助脚本启动连接，保留 stdin/stdout 传输剪贴板数据。
	cmd, source, err := s.clipboardCommand(ctx, "stream", clipboardFileSelection{})
	if err != nil {
		http.Error(w, "剪贴板客机配置无效", 502)
		return
	}
	in, err := cmd.StdinPipe()
	if err != nil {
		http.Error(w, "剪贴板连接失败", 502)
		return
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		in.Close()
		http.Error(w, "剪贴板连接失败", 502)
		return
	}
	if err = cmd.Start(); err != nil {
		in.Close()
		out.Close()
		http.Error(w, "剪贴板连接失败", 502)
		return
	}
	defer func() { cancel(); in.Close(); out.Close(); _ = cmd.Wait() }()
	ws, err := s.upgrade(w, r)
	if err != nil {
		return
	}
	defer ws.Close()
	ws.SetReadLimit(clipboardWireLimit)
	var writeMu sync.Mutex
	writeGuest := func(payload clipboardPayload) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		timeout := time.AfterFunc(8*time.Second, func() { cancel(); in.Close() })
		defer timeout.Stop()
		return json.NewEncoder(in).Encode(payload)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer ws.Close()
		scanner := bufio.NewScanner(out)
		scanner.Buffer(make([]byte, 64<<10), clipboardWireLimit)
		for scanner.Scan() {
			var payload clipboardPayload
			if json.Unmarshal(scanner.Bytes(), &payload) != nil {
				return
			}
			if payload.Hello != nil {
				hello := payload.Hello
				expectedOS := "linux"
				if source == "guest" {
					expectedOS = "windows"
					if s.macosGuest != nil {
						expectedOS = "darwin"
					}
				}
				if hello.Version != 1 || hello.OS != expectedOS || len(hello.Session) == 0 || len(hello.Session) > 256 {
					return
				}
				s.mu.Lock()
				changed := s.clipboardGuestSession != "" && s.clipboardGuestSession != hello.Session
				s.mu.Unlock()
				if changed {
					s.clearClipboardFiles()
				}
				s.mu.Lock()
				s.clipboardGuestSession = hello.Session
				if hello.FileRelease {
					s.clipboardRelease = func(paths []string) { _ = writeGuest(clipboardPayload{Release: paths}) }
				} else {
					s.clipboardRelease = nil
				}
				s.mu.Unlock()
			}
			if payload.Files != nil {
				var paths []string
				if json.Unmarshal(payload.Files, &paths) != nil || len(paths) == 0 || len(paths) > 100 {
					return
				}
				if payload.Source != source && !(source == "cage" && payload.Source == "cage-browser") {
					return
				}
				payload.Files, _ = json.Marshal(s.registerClipboardFiles(paths, payload.Source))
				payload.Source = ""
			}
			_ = ws.SetWriteDeadline(time.Now().Add(15 * time.Second))
			if ws.WriteJSON(payload) != nil {
				return
			}
		}
	}()
	defer func() {
		cancel()
		in.Close()
		out.Close()
		ws.Close()
		<-done
		s.clearClipboardFiles()
	}()
	for {
		var payload clipboardPayload
		if ws.ReadJSON(&payload) != nil {
			return
		}
		if payload.Release != nil {
			return
		}
		if payload.Files != nil {
			var id string
			if json.Unmarshal(payload.Files, &id) != nil {
				return
			}
			paths := s.clipboardFileSelection(id).Paths
			if len(paths) == 0 {
				return
			}
			payload.Files, _ = json.Marshal(paths)
		}
		if writeGuest(payload) != nil {
			return
		}
	}
}

func (s *Server) clipboardArchive(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		s.releaseClipboardFiles(r.PathValue("id"))
		jsonReply(w, map[string]bool{"ok": true})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()
	if !s.vmRunning() {
		http.Error(w, "请先启动环境", 409)
		return
	}
	if r.Method == "GET" {
		selection, release := s.acquireClipboardFiles(r.PathValue("id"))
		defer release()
		paths := selection.Paths
		if len(paths) == 0 {
			http.Error(w, "剪贴板文件已过期，请重新复制", 404)
			return
		}
		// Export with the copying application's permissions. A URI written by
		// a guest application cannot grant access to root-only files.
		cmd, _, err := s.clipboardCommand(ctx, "export", selection)
		if err != nil {
			http.Error(w, "读取剪贴板文件失败", 502)
			return
		}
		out, err := cmd.StdoutPipe()
		if err != nil || cmd.Start() != nil {
			http.Error(w, "读取剪贴板文件失败", 502)
			return
		}
		defer out.Close()
		w.Header().Set("Content-Type", "application/x-tar")
		w.Header().Set("Trailer", "X-Cage-Clipboard-Status")
		_, copied := io.Copy(w, out)
		if copied != nil {
			cancel()
		}
		finished := cmd.Wait()
		status := "failed"
		if copied == nil && finished == nil {
			status = "ok"
		}
		w.Header().Set("X-Cage-Clipboard-Status", status)
		return
	}
	cmd, source, err := s.clipboardCommand(ctx, "import", clipboardFileSelection{})
	if err != nil {
		http.Error(w, "传递剪贴板文件失败", 502)
		return
	}
	cmd.Stdin = http.MaxBytesReader(w, r.Body, 201<<30)
	out, err := cmd.StdoutPipe()
	if err != nil || cmd.Start() != nil {
		http.Error(w, "传递剪贴板文件失败", 502)
		return
	}
	data, err := io.ReadAll(io.LimitReader(out, 65537))
	out.Close()
	finished := cmd.Wait()
	var result struct {
		Paths []string `json:"paths"`
	}
	if err != nil || finished != nil || len(data) > 65536 || json.Unmarshal(data, &result) != nil || len(result.Paths) == 0 {
		http.Error(w, "传递剪贴板文件失败，请核对文件大小和名称", 502)
		return
	}
	jsonReply(w, map[string]string{"files": s.registerClipboardFiles(result.Paths, source)})
}

// clipboardCopy reads only the new text produced by an explicit web copy.
// The UI sends the keys on its existing window input queue between both phases.
func (s *Server) clipboardCopy(w http.ResponseWriter, r *http.Request) {
	if s.macosGuest == nil {
		http.NotFound(w, r)
		return
	}
	var request struct {
		Phase    string `json:"phase"`
		Session  string `json:"session"`
		Revision *int   `json:"revision"`
	}
	if !readJSON(w, r, &request) {
		return
	}
	if request.Phase != "prepare" && request.Phase != "read" ||
		request.Phase == "read" && (request.Revision == nil || *request.Revision < 0 || len(request.Session) == 0 || len(request.Session) > 256) {
		http.Error(w, "剪贴板复制请求无效", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	cmd, _, err := s.clipboardCommand(ctx, "request", clipboardFileSelection{})
	if err != nil {
		http.Error(w, "无法连接客机剪贴板", http.StatusBadGateway)
		return
	}
	body, _ := json.Marshal(map[string]any{"operation": "copy-" + request.Phase, "revision": request.Revision, "session": request.Session})
	cmd.Stdin = bytes.NewReader(append(body, '\n'))
	out, err := cmd.StdoutPipe()
	if err != nil || cmd.Start() != nil {
		http.Error(w, "无法连接客机剪贴板", http.StatusBadGateway)
		return
	}
	data, readErr := io.ReadAll(io.LimitReader(out, clipboardWireLimit+1))
	out.Close()
	finished := cmd.Wait()
	var result struct {
		Text     *string `json:"text,omitempty"`
		Session  string  `json:"session"`
		Revision int     `json:"revision"`
		Error    string  `json:"error,omitempty"`
	}
	if readErr != nil || finished != nil || len(data) > clipboardWireLimit || json.Unmarshal(data, &result) != nil || result.Error != "" ||
		len(result.Session) == 0 || request.Phase == "read" && (result.Text == nil || len(*result.Text) > 2<<20 || result.Session != request.Session || result.Revision == *request.Revision) {
		http.Error(w, "未收到新的选中文本，请确认客机已解锁并重新复制", http.StatusConflict)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	jsonReply(w, result)
}

func writeClipboardArchive(output io.Writer, paths []string) error {
	return clipboardfiles.Write(output, paths)
}

func readClipboardArchive(input io.Reader) ([]string, error) {
	return clipboardfiles.Read(input)
}

// A tar EOF only ends the archive, not the guest process. Drain the HTTP body
// before checking the producer's completion trailer or publishing local paths.
func receiveClipboardArchive(response *http.Response, cache clipboardfiles.CacheStore) (clipboardfiles.CachedSelection, error) {
	selection, err := cache.Read(response.Body)
	if err == nil {
		_, err = io.Copy(io.Discard, response.Body)
	}
	if err == nil && response.Trailer.Get("X-Cage-Clipboard-Status") != "ok" {
		err = errors.New("剪贴板文件传递中断，请重新复制")
	}
	if err != nil {
		if selection.ID != "" {
			_ = cache.Release(selection.ID)
		}
		return clipboardfiles.CachedSelection{}, err
	}
	return selection, nil
}

// Native helpers stream bytes directly, keeping host paths out of the WebView.
func ClipboardFilesForConnection(configPath, connectionID, operation string, args []string) (map[string]any, error) {
	cache, err := clipboardfiles.UserCacheStore()
	if err != nil {
		return nil, err
	}
	if operation == "cache" && len(args) == 1 {
		var request clipboardfiles.CacheRequest
		if json.Unmarshal([]byte(args[0]), &request) != nil {
			return nil, errors.New("剪贴板缓存请求无效")
		}
		return cache.Control(request)
	}
	client, err := connectController(filepath.Dir(configPath))
	if err != nil {
		return nil, err
	}
	defer client.close()
	entry, _ := url.Parse(client.launch.URL)
	status, err := client.call("POST", "/api/session", map[string]string{"token": entry.Fragment}, nil)
	if err != nil || status != http.StatusOK {
		return nil, errors.New("无法连接剪贴板会话")
	}
	if _, err = client.bindConnection(connectionID); err != nil {
		return nil, err
	}
	client.client.Timeout = 30 * time.Minute
	path := client.origin + "/api/clipboard/files"
	var body io.Reader
	method := "POST"
	if (operation == "receive" || operation == "release") && len(args) == 1 && browserFileID.MatchString(args[0]) {
		path += "/" + args[0]
		method = "GET"
		if operation == "release" {
			method = http.MethodDelete
			client.client.Timeout = 5 * time.Second
		}
	} else if operation == "send" {
		reader, writer := io.Pipe()
		defer reader.Close()
		body = reader
		go func() { writer.CloseWithError(writeClipboardArchive(writer, args)) }()
	} else {
		return nil, errors.New("剪贴板文件操作无效")
	}
	request, err := http.NewRequest(method, path, body)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Origin", client.origin)
	request.Header.Set("Content-Type", "application/x-tar")
	response, err := client.client.Do(request)
	if err != nil {
		return nil, errors.New("剪贴板文件传递中断，请重新复制")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, errors.New("剪贴板文件传递失败，请重新复制")
	}
	if method == "GET" {
		selection, err := receiveClipboardArchive(response, cache)
		if err != nil {
			return nil, err
		}
		return map[string]any{"paths": selection.Paths, "cache_id": selection.ID}, nil
	}
	var result map[string]any
	err = json.NewDecoder(io.LimitReader(response.Body, 65537)).Decode(&result)
	return result, err
}
