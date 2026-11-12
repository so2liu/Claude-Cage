package workbench

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/sylvinus/agent-vm/internal/diagnostics"
	"github.com/sylvinus/agent-vm/internal/guestos"
)

// windowsBrowserClient owns a viewer's authenticated relay connection. The GUI
// agent owns Chrome, so closing this client preserves tabs and unsaved editors.
type windowsBrowserClient struct {
	ownsBrowser bool
	mu          sync.Mutex
	ctx         context.Context
	cancel      context.CancelFunc
	start       func(context.Context) (*exec.Cmd, error)
	cmd         *exec.Cmd
	input       io.WriteCloser
	output      *bufio.Scanner
}

// Keep the guest's structured failure code: the display can distinguish a
// rejected input from a broken or untrusted frame transport.
type windowsBrowserFailure struct{ code, message string }

func (e windowsBrowserFailure) Error() string { return e.message }

func newWindowsBrowserClient(parent context.Context, profile string) *windowsBrowserClient {
	ctx, cancel := context.WithCancel(parent)
	return &windowsBrowserClient{ctx: ctx, cancel: cancel, start: func(ctx context.Context) (*exec.Cmd, error) {
		binding, err := guestos.LoadWindowsProfile(profile)
		if err != nil {
			return nil, err
		}
		if binding == nil {
			return nil, errors.New("此配置尚未绑定 客机")
		}
		return exec.CommandContext(ctx, binding.RelayExecutable, "--relay-profile", guestos.WindowsProfilePath(profile), "browser"), nil
	}}
}

// A modern guest service owns the browser. Its viewers use separate relay
// connections so cancellation also releases each viewer's held input.
func (c *windowsBrowserClient) viewer(parent context.Context) (*windowsBrowserClient, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ownsBrowser {
		return c, func() {}
	}
	ctx, cancel := context.WithCancel(parent)
	viewer := &windowsBrowserClient{ctx: ctx, cancel: cancel, start: c.start, ownsBrowser: true}
	return viewer, viewer.close
}

var windowsBrowserRoutes = map[string]bool{
	"tailnet": true, "health": true, "tabs": true, "events": true, "frame": true, "new": true, "activate": true, "close": true,
	"navigate": true, "back": true, "forward": true, "reload": true, "homepage": true, "resize": true,
	"insert-text": true, "copy-context": true, "ime": true, "copy": true, "cut": true, "paste": true, "select-all": true,
	"key": true, "pointer": true, "click": true, "wheel": true, "input": true, "editor": true, "editor-restart": true,
	"upload-start": true, "upload-finish": true, "upload-cancel": true, "download-retry": true, "info": true, "cookies": true,
	"settings": true, "internal-page": true, "extensions": true, "split": true, "audit": true, "vault": true, "login-open": true, "login-finish": true,
	"file-open": true, "file-chunk": true, "file-finish": true, "file-read": true, "file-close": true,
	"image-start": true, "image-chunk": true, "image-finish": true, "image-cancel": true,
	"paste-start": true, "paste-chunk": true, "paste-finish": true, "paste-cancel": true,
	"phone-browser/open": true, "phone-browser/frame": true, "phone-browser/action": true, "phone-browser/close": true,
}

type windowsPhoneBrowserError struct{ status int }

func (e windowsPhoneBrowserError) Error() string {
	return "手机浏览器操作未完成，请核对授权和最新画面"
}

func (c *windowsBrowserClient) pasteImage(ctx context.Context, id string, png []byte, transfer string) ([]byte, error) {
	return c.pasteData(ctx, id, png, transfer, "image/png")
}

func (c *windowsBrowserClient) pasteData(ctx context.Context, id string, data []byte, transfer, mime string) ([]byte, error) {
	limit := maxImageBytes
	if mime == "text/plain" {
		limit = 2 << 20
	}
	if !validInputToken(id) || len(data) == 0 || len(data) > limit || (mime != "text/plain" && mime != "image/png") {
		return nil, browserInputError{errors.New("图片或浏览器目标无效")}
	}
	if transfer == "" {
		reply, err := c.request(ctx, "POST", "/paste-start", map[string]any{"id": id, "length": len(data), "mime": mime})
		if err != nil {
			return nil, err
		}
		var opened struct {
			Transfer string `json:"transfer"`
		}
		if json.Unmarshal(reply, &opened) != nil {
			return nil, errors.New("客机图片接收会话无效")
		}
		transfer = opened.Transfer
	}
	if !validInputToken(transfer) {
		return nil, errors.New("客机图片接收会话无效")
	}
	completed := false
	defer func() {
		if !completed {
			_, _ = c.request(ctx, "POST", "/paste-cancel", map[string]any{"transfer": transfer})
		}
	}()
	sequence := 0
	for offset := 0; offset < len(data); offset += 64 << 10 {
		end := offset + (64 << 10)
		if end > len(data) {
			end = len(data)
		}
		chunk, err := c.request(ctx, "POST", "/paste-chunk", map[string]any{"transfer": transfer, "sequence": sequence, "length": len(data), "mime": mime, "data": base64.StdEncoding.EncodeToString(data[offset:end])})
		if err != nil {
			return nil, err
		}
		var ack struct {
			Ack int `json:"ack"`
		}
		if json.Unmarshal(chunk, &ack) != nil || ack.Ack != sequence+1 {
			return nil, errors.New("客机图片数据接收未确认")
		}
		sequence++
	}
	result, err := c.request(ctx, "POST", "/paste-finish", map[string]any{"transfer": transfer, "sequence": sequence})
	if err == nil {
		var receipt struct {
			OK bool `json:"ok"`
		}
		if json.Unmarshal(result, &receipt) != nil || !receipt.OK {
			err = errors.New("客机图片粘贴未确认")
		}
	}
	completed = err == nil
	return result, err
}

func validWindowsBrowserRoute(method, path string) bool {
	if !strings.HasPrefix(path, "/") || !windowsBrowserRoutes[strings.TrimPrefix(path, "/")] {
		return false
	}
	return method == "POST" || method == "GET" && (path == "/health" || path == "/tabs" || path == "/events" || path == "/frame")
}

func (c *windowsBrowserClient) disconnectLocked() {
	if c.input != nil {
		_ = c.input.Close()
	}
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
		_ = c.cmd.Wait()
	}
	c.cmd, c.input, c.output = nil, nil, nil
}

func (c *windowsBrowserClient) close() {
	c.cancel()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.disconnectLocked()
}

func (c *windowsBrowserClient) releaseViewerInput() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.disconnectLocked()
}

func (c *windowsBrowserClient) request(ctx context.Context, method, path string, input any) ([]byte, error) {
	if !validWindowsBrowserRoute(method, path) {
		return nil, browserInputError{errors.New("客机浏览器操作无效")}
	}
	body, err := json.Marshal(struct {
		Method string `json:"method"`
		Path   string `json:"path"`
		Input  any    `json:"input,omitempty"`
	}{method, path, input})
	if err != nil || len(body) > 128<<10 {
		return nil, browserInputError{errors.New("客机浏览器请求过大或格式无效")}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := c.ctx.Err(); err != nil {
		return nil, err
	}
	if c.cmd == nil {
		cmd, err := c.start(c.ctx)
		if err != nil {
			return nil, err
		}
		in, err := cmd.StdinPipe()
		if err != nil {
			return nil, err
		}
		out, err := cmd.StdoutPipe()
		if err != nil {
			_ = in.Close()
			return nil, err
		}
		if err := cmd.Start(); err != nil {
			_ = in.Close()
			_ = out.Close()
			return nil, errors.New("无法连接 客机浏览器，请核对 GUI 登录、配对与部署组件")
		}
		c.cmd, c.input = cmd, in
		c.output = bufio.NewScanner(out)
		c.output.Buffer(make([]byte, 64<<10), 30<<20)
	}
	// macOS runs the browser over one SSH pipe shared by the workbench panes.
	// Finish the in-flight reply even if a viewer leaves, preserving editor buffers.
	if c.ownsBrowser {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(c.ctx, 40*time.Second)
		defer cancel()
	}
	process := c.cmd.Process
	callbackDone := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(callbackDone)
		_ = process.Kill()
	})
	defer func() {
		if !stop() {
			<-callbackDone
		}
	}()
	if _, err = c.input.Write(append(body, '\n')); err != nil || !c.output.Scan() {
		c.disconnectLocked()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("客机浏览器连接已关闭，请恢复连接后核对操作结果")
	}
	data := append([]byte(nil), c.output.Bytes()...)
	if err := ctx.Err(); err != nil {
		c.disconnectLocked()
		return nil, err
	}
	var result struct {
		Error   string `json:"error"`
		Code    string `json:"code"`
		Status  int    `json:"status"`
		Service *struct {
			Version    int  `json:"version"`
			Persistent bool `json:"persistent"`
		} `json:"service"`
	}
	if json.Unmarshal(data, &result) != nil {
		c.disconnectLocked()
		return nil, errors.New("客机浏览器返回了无效结果")
	}
	if result.Service != nil && result.Service.Version == 1 && result.Service.Persistent {
		c.ownsBrowser = false
	}
	if result.Error != "" {
		if strings.HasPrefix(path, "/phone-browser/") {
			status := result.Status
			if status != 400 && status != 403 && status != 409 && status != 410 && status != 425 && status != 501 && status != 503 {
				status = 503
			}
			return nil, windowsPhoneBrowserError{status: status}
		}
		known := map[string]string{
			"network_unavailable":         "客机受控网络代理尚未配置",
			"browser_network_unavailable": "客机受控网络代理尚未配置",
			"browser_unavailable":         "客机图形浏览器未就绪，请核对 GUI 登录与组件安装",
			"browser_components_missing":  "客机浏览器组件尚未完整安装",
			"requires_gui_login":          "请先登录并解锁 客机桌面",
			"editor_unavailable":          "客机编辑器或项目授权尚未接通",
			"editor_components_missing":   "客机编辑器运行组件尚未完整安装",
			"project_grant_unavailable":   "此项目授权已断开，请重新连接后再操作",
			"extensions_unavailable":      "客机浏览器扩展管理未就绪，请核对浏览器版本和安装组件",
			"clipboard_copy_not_observed": "尚未确认客机剪贴板发生复制，请重新选择内容后复制",
			"copy_unavailable":            "未收到新的复制内容，请重新选择后复制",
			"copy_changed":                "复制来源已切换，请重新复制",
			"copy_limit":                  "复制内容超过大小限制",
			"copy_image_invalid":          "复制的 PNG 图片无效",
			"action_unavailable":          "客机浏览器尚未支持这项操作",
			"target_changed":              "接收操作的网页已经切换，请回到原网页重试",
			"browser_target_changed":      "客机浏览器窗口已变化，请在新画面中重试",
			"browser_focus_required":      "客机浏览器未能获得前台焦点，请先激活浏览器或关闭阻挡操作的弹窗",
			"desktop_target_changed":      "弹窗正在切换，正在重新读取画面",
			"desktop_capture_busy":        "弹窗画面正在准备",
			"desktop_empty_frame":         "弹窗尚未返回画面",
			"frame_unavailable":           "浏览器画面流尚未就绪",
			"transfer_changed":            "文件传递已经关闭，请重新选择文件",
			"frame_limit":                 "浏览器画面过大，请缩小窗口后重试",
		}
		code := result.Code
		if code == "" {
			code = result.Error
		}
		if len(code) <= 80 && strings.IndexFunc(code, func(r rune) bool { return r != '_' && (r < 'a' || r > 'z') }) == -1 {
			diagnostics.Log("warn", "native-browser", method+" "+path+" failed ("+code+")")
		}
		if reason := known[code]; reason != "" {
			return nil, windowsBrowserFailure{code: code, message: reason}
		}
		return nil, errors.New("客机浏览器操作未完成，请恢复连接后核对结果")
	}
	return data, nil
}

func (c *windowsBrowserClient) upload(ctx context.Context, id, name string, body io.Reader, length int64) ([]byte, error) {
	if !browserFileID.MatchString(id) || !browserFileName(name) || length < -1 || length > browserFileLimit {
		return nil, browserInputError{errors.New("上传文件名、大小或窗口无效")}
	}
	result, received, err := c.sendUpload(ctx, map[string]any{"direction": "upload", "id": id, "name": name, "length": length}, body, length, browserFileLimit)
	if err == nil {
		var receipt struct {
			OK   bool   `json:"ok"`
			Size int64  `json:"size"`
			Name string `json:"name"`
		}
		if json.Unmarshal(result, &receipt) != nil || !receipt.OK || receipt.Size != received || receipt.Name != name {
			err = errors.New("客机文件接收未确认")
		}
	}
	return result, err
}

func (c *windowsBrowserClient) uploadExtension(ctx context.Context, body io.Reader, length int64) ([]byte, error) {
	if length < -1 || length > 32<<20 {
		return nil, browserInputError{errors.New("扩展 ZIP 不能超过 32 MB")}
	}
	result, _, err := c.sendUpload(ctx, map[string]any{"direction": "extension", "length": length}, body, length, 32<<20)
	if err == nil {
		var preview struct {
			Session string `json:"session"`
			Name    string `json:"name"`
			Version string `json:"version"`
		}
		if json.Unmarshal(result, &preview) != nil || !browserFileID.MatchString(preview.Session) || preview.Name == "" || preview.Version == "" {
			err = errors.New("客机没有返回有效的扩展权限预览")
		}
	}
	return result, err
}

func (c *windowsBrowserClient) sendUpload(ctx context.Context, input map[string]any, body io.Reader, length, limit int64) ([]byte, int64, error) {
	data, err := c.request(ctx, "POST", "/file-open", input)
	if err != nil {
		return nil, 0, err
	}
	var opened struct {
		Transfer string `json:"transfer"`
	}
	if json.Unmarshal(data, &opened) != nil || !validInputToken(opened.Transfer) {
		return nil, 0, errors.New("客机没有返回有效的上传会话")
	}
	completed := false
	defer func() {
		if !completed {
			// A failed upload's request context may already be canceled. The
			// guest also expires abandoned transfers; don't extend shutdown.
			_, _ = c.request(ctx, "POST", "/file-close", map[string]any{"transfer": opened.Transfer})
		}
	}()
	buffer := make([]byte, 64<<10)
	var received int64
	sequence := 0
	for {
		count, readErr := io.ReadFull(body, buffer)
		if count > 0 {
			received += int64(count)
			if received > limit || length >= 0 && received > length {
				return nil, received, errors.New("上传文件超过声明大小或大小限制")
			}
			response, err := c.request(ctx, "POST", "/file-chunk", map[string]any{"transfer": opened.Transfer, "sequence": sequence, "data": base64.StdEncoding.EncodeToString(buffer[:count])})
			if err != nil {
				return nil, received, err
			}
			var ack struct {
				Ack int `json:"ack"`
			}
			if json.Unmarshal(response, &ack) != nil || ack.Ack != sequence+1 {
				return nil, received, errors.New("客机文件接收确认无效")
			}
			sequence++
		}
		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			break
		}
		if readErr != nil {
			return nil, received, readErr
		}
	}
	if length >= 0 && received != length {
		return nil, received, errors.New("上传文件没有完整读入")
	}
	result, err := c.request(ctx, "POST", "/file-finish", map[string]any{"transfer": opened.Transfer, "sequence": sequence})
	completed = err == nil
	return result, received, err
}

type windowsBrowserDownload struct {
	request   func(context.Context, string, string, any) ([]byte, error)
	ctx       context.Context
	transfer  string
	sequence  int
	buffer    []byte
	remaining int64
	eof       bool
	closed    bool
}

func (c *windowsBrowserClient) download(ctx context.Context, id string) (io.ReadCloser, int64, string, error) {
	return openNativeFileDownload(ctx, id, c.request)
}

func (c *windowsBrowserClient) downloadClipboard(ctx context.Context, input browserClipboardInput) (io.ReadCloser, int64, string, error) {
	var metadata struct {
		Mime string `json:"mime"`
	}
	request := func(ctx context.Context, method, path string, value any) ([]byte, error) {
		if path == "/file-open" {
			value = map[string]any{"direction": "download", "id": input.ID, "tab": input.Tab, "context": input.Context, "document": input.Document}
		}
		data, err := c.request(ctx, method, path, value)
		if err == nil && path == "/file-open" {
			_ = json.Unmarshal(data, &metadata)
		}
		return data, err
	}
	reader, size, _, err := openNativeFileDownload(ctx, input.ID, request)
	return reader, size, metadata.Mime, err
}

func openNativeFileDownload(ctx context.Context, id string, request func(context.Context, string, string, any) ([]byte, error)) (io.ReadCloser, int64, string, error) {
	if !browserFileID.MatchString(id) {
		return nil, 0, "", browserInputError{errors.New("下载标识无效")}
	}
	data, err := request(ctx, "POST", "/file-open", map[string]any{"direction": "download", "id": id})
	if err != nil {
		return nil, 0, "", err
	}
	var opened struct {
		Transfer string `json:"transfer"`
		Size     int64  `json:"size"`
		Name     string `json:"name"`
	}
	if json.Unmarshal(data, &opened) != nil || !validInputToken(opened.Transfer) || opened.Size < 0 || opened.Size > browserFileLimit || !browserDownloadName(opened.Name) {
		if validInputToken(opened.Transfer) {
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			defer cancel()
			_, _ = request(cleanup, "POST", "/file-close", map[string]any{"transfer": opened.Transfer})
		}
		return nil, 0, "", errors.New("客机没有返回有效的下载会话")
	}
	return &windowsBrowserDownload{request: request, ctx: ctx, transfer: opened.Transfer, remaining: opened.Size}, opened.Size, opened.Name, nil
}

func (d *windowsBrowserDownload) Read(output []byte) (int, error) {
	if len(output) == 0 {
		return 0, nil
	}
	if d.closed {
		return 0, io.ErrClosedPipe
	}
	if len(d.buffer) == 0 && !d.eof {
		data, err := d.request(d.ctx, "POST", "/file-read", map[string]any{"transfer": d.transfer, "sequence": d.sequence})
		if err != nil {
			return 0, err
		}
		var chunk struct {
			Data     string `json:"data"`
			Sequence int    `json:"sequence"`
			EOF      bool   `json:"eof"`
		}
		if json.Unmarshal(data, &chunk) != nil || chunk.Sequence != d.sequence+1 || len(chunk.Data) > 87384 {
			return 0, errors.New("客机下载数据块无效")
		}
		bytes, err := base64.StdEncoding.Strict().DecodeString(chunk.Data)
		if err != nil || len(bytes) > 64<<10 || int64(len(bytes)) > d.remaining || len(bytes) == 0 && !chunk.EOF {
			return 0, errors.New("客机下载数据块无效")
		}
		d.remaining -= int64(len(bytes))
		if chunk.EOF && d.remaining != 0 || !chunk.EOF && d.remaining == 0 {
			return 0, io.ErrUnexpectedEOF
		}
		d.buffer, d.eof = bytes, chunk.EOF
		d.sequence++
	}
	count := copy(output, d.buffer)
	d.buffer = d.buffer[count:]
	if count == 0 && d.eof {
		return 0, io.EOF
	}
	return count, nil
}

func (d *windowsBrowserDownload) Close() error {
	if d.closed {
		return nil
	}
	d.closed = true
	if d.eof {
		return nil
	}
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(d.ctx), 3*time.Second)
	defer cancel()
	_, err := d.request(cleanup, "POST", "/file-close", map[string]any{"transfer": d.transfer})
	return err
}
