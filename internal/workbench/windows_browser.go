package workbench

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"time"
	"unicode/utf8"

	"github.com/sylvinus/agent-vm/internal/diagnostics"
)

func (s *Server) nativeBrowserClient() *windowsBrowserClient {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.windowsBrowserControl == nil {
		if s.macosGuest != nil {
			ctx, cancel := context.WithCancel(s.lifetimeContextLocked())
			s.windowsBrowserControl = &windowsBrowserClient{ctx: ctx, cancel: cancel, ownsBrowser: true, start: func(ctx context.Context) (*exec.Cmd, error) {
				const base = "/usr/local/libexec/cage-workbench-browser-4.145.0-pw1.65.0/"
				return s.macOSSSHCommand(ctx, "-T", s.sshTarget(), "--", "exec "+base+"editor/lib/node "+base+"controller/macos-native-browser.mjs"), nil
			}}
		} else {
			s.windowsBrowserControl = newWindowsBrowserClient(s.lifetimeContextLocked(), s.profile)
		}
	}
	return s.windowsBrowserControl
}

func (s *Server) closeWindowsBrowser() {
	s.mu.Lock()
	browser := s.windowsBrowserControl
	s.windowsBrowserControl = nil
	s.mu.Unlock()
	if browser != nil {
		browser.close()
	}
}

func (s *Server) nativeBrowserAPI(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	if method == http.MethodPost && path == "/paste-image" {
		var input struct {
			ID       string `json:"id"`
			PNG      string `json:"png"`
			Transfer string `json:"transfer"`
		}
		if json.Unmarshal(body, &input) != nil {
			return nil, browserInputError{errors.New("图片粘贴请求无效")}
		}
		data, err := decodeInputImage(input.PNG)
		if err != nil {
			return nil, browserInputError{err}
		}
		return s.nativeBrowserClient().pasteImage(ctx, input.ID, data, input.Transfer)
	}
	var input any
	if len(body) != 0 && json.Unmarshal(body, &input) != nil {
		return nil, browserInputError{errors.New("浏览器请求无效")}
	}
	return s.nativeBrowserClient().request(ctx, method, path, input)
}

type browserClipboardInput struct {
	ID       string `json:"id"`
	Tab      string `json:"tab"`
	Context  string `json:"context"`
	Document string `json:"document"`
}

func (s *Server) browserClipboard(w http.ResponseWriter, r *http.Request) {
	var input browserClipboardInput
	if !readJSON(w, r, &input) {
		return
	}
	if !browserFileID.MatchString(input.ID) || !validInputToken(input.Tab) || !validInputToken(input.Context) || !validInputToken(input.Document) {
		http.Error(w, "复制内容或网页上下文无效", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	ready := s.ready
	s.mu.Unlock()
	if !ready {
		http.Error(w, "环境尚未就绪", http.StatusConflict)
		return
	}
	if s.macosGuest == nil {
		http.Error(w, "当前客机使用系统共享剪贴板", http.StatusNotImplemented)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	reader, size, mime, err := s.nativeBrowserClient().downloadClipboard(ctx, input)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer reader.Close()
	limit := int64(maxImageBytes)
	if mime == "text/plain" {
		limit = 2 << 20
	}
	if (mime != "image/png" && mime != "text/plain") || size > limit {
		http.Error(w, "客机复制内容的类型或大小无效", http.StatusBadGateway)
		return
	}
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil || int64(len(data)) != size {
		http.Error(w, "复制内容未完整收到，请重新复制", http.StatusBadGateway)
		return
	}
	if mime == "text/plain" {
		if !utf8.Valid(data) {
			http.Error(w, "复制文字的编码无效", http.StatusBadGateway)
			return
		}
		jsonReply(w, map[string]string{"text": string(data)})
		return
	}
	if _, err := validateInputImage(data); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	jsonReply(w, map[string]string{"png": base64.StdEncoding.EncodeToString(data)})
}

func (s *Server) subscribeNativeBrowser(parent context.Context) io.ReadCloser {
	ctx, cancel := context.WithCancel(parent)
	reader, writer := io.Pipe()
	go func() {
		defer writer.Close()
		defer cancel()
		ticker := time.NewTicker(300 * time.Millisecond)
		defer ticker.Stop()
		var previous string
		var latest json.RawMessage
		var sequence uint64
		var downloadRevision uint64
		for {
			body, _ := json.Marshal(map[string]any{"after": sequence, "onlyChanges": true})
			data, err := s.nativeBrowserAPI(ctx, "GET", "/events", body)
			if err != nil {
				_ = writer.CloseWithError(err)
				return
			}
			var update struct {
				Sequence uint64          `json:"sequence"`
				State    json.RawMessage `json:"state"`
				Events   []struct {
					Sequence uint64          `json:"sequence"`
					Type     string          `json:"type"`
					Value    json.RawMessage `json:"value"`
				} `json:"events"`
			}
			if err = json.Unmarshal(data, &update); err != nil {
				_ = writer.CloseWithError(err)
				return
			}
			if update.Sequence < sequence {
				// A restarted guest service has a new event sequence. Read its
				// retained events before advancing this subscription's cursor.
				sequence, previous, latest = 0, "", nil
				continue
			}
			for _, event := range update.Events {
				if event.Sequence <= sequence || event.Sequence > update.Sequence {
					continue
				}
				switch event.Type {
				case "clipboard", "native-clipboard", "copy-expired":
					if len(event.Value) == 0 {
						continue
					}
					if _, err = fmt.Fprintf(writer, "id: %d\nevent: %s\ndata: %s\n\n", event.Sequence, event.Type, event.Value); err != nil {
						return
					}
				}
			}
			sequence = update.Sequence
			if len(update.State) != 0 {
				latest = update.State
			}
			s.mu.Lock()
			revision := s.nativeDownloadRevision
			s.mu.Unlock()
			// Host copies finish independently of browser events. Re-emit the
			// current guest snapshot so the common mapper publishes that result.
			if len(latest) != 0 && (string(latest) != previous || revision != downloadRevision) {
				if _, err = writer.Write(append(append([]byte("data: "), latest...), '\n', '\n')); err != nil {
					return
				}
				previous, downloadRevision = string(latest), revision
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return &windowsBrowserEventStream{ReadCloser: reader, cancel: cancel}
}

type windowsBrowserEventStream struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (s *windowsBrowserEventStream) Close() error { s.cancel(); return s.ReadCloser.Close() }

func (s *Server) nativeBrowserFile(w http.ResponseWriter, r *http.Request, method, path string, body io.Reader) {
	request, err := url.Parse(path)
	if err != nil {
		http.Error(w, "浏览器文件操作无效", 400)
		return
	}
	query := request.Query()
	if method == "POST" && request.Path == "/upload-file" {
		result, err := s.nativeBrowserClient().upload(r.Context(), query.Get("id"), query.Get("name"), body, r.ContentLength)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(result)
		return
	}
	if method == "GET" && request.Path == "/download-file" {
		reader, size, _, err := s.nativeBrowserClient().download(r.Context(), query.Get("id"))
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		defer reader.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		if size < 0 {
			http.Error(w, "文件大小无效", 502)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(size))
		_, _ = io.Copy(w, reader)
		return
	}
	http.Error(w, "Windows 浏览器文件操作尚未接通", 501)
}

func (s *Server) nativeBrowserVNC(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	var client *windowsBrowserClient
	release := func() {}
	if s.macosGuest != nil {
		client = s.nativeBrowserClient()
	} else {
		client = newWindowsBrowserClient(ctx, s.profile)
		release = client.close
	}
	defer func() { release() }()
	var current windowsWindowFrame
	request := func(input map[string]any, output any) error {
		var data []byte
		var err error
		switch input["action"] {
		case "frame":
			var next windowsWindowFrame
			next, err = readNativeBrowserFrame(ctx, func() ([]byte, error) {
				return client.request(ctx, "GET", "/frame", input)
			})
			if err == nil {
				if !next.Unchanged {
					current = next
				}
				*output.(*windowsWindowFrame) = next
			}
			return err
		case "pointer", "key":
			input["kind"] = input["action"]
			delete(input, "action")
			s.mu.Lock()
			keyboard := s.preferences.Keyboard
			s.mu.Unlock()
			if input["kind"] == "key" && s.windowsGuest != nil {
				if key, ok := input["keysym"].(uint32); ok {
					input["keysym"] = windowsGuestKeysym(key, keyboard)
				}
			}
			data, err = client.request(ctx, "POST", "/input", input)
		case "text":
			data, err = client.request(ctx, "POST", "/insert-text", map[string]any{"id": current.ID, "document": current.Document, "text": input["text"]})
		default:
			return errors.New("浏览器画面操作无效")
		}
		if err != nil {
			return err
		}
		if output != nil {
			return json.Unmarshal(data, output)
		}
		return nil
	}
	if err := request(map[string]any{"action": "frame"}, &current); err != nil {
		http.Error(w, err.Error(), 409)
		return
	}
	if s.macosGuest != nil {
		owner := client
		client, release = owner.viewer(ctx)
		if client != owner {
			if err := request(map[string]any{"action": "frame"}, &current); err != nil {
				http.Error(w, err.Error(), 409)
				return
			}
		}
	}
	local, remote := net.Pipe()
	defer local.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer remote.Close()
		err := serveWindowsWindowRFB(remote, current, request)
		if err != nil && ctx.Err() == nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) && !errors.Is(err, net.ErrClosed) {
			_ = diagnostics.New(s.profile).Record("warn", "native-browser-display", err.Error())
		}
	}()
	s.streamVNC(w, r, local, local, func() { cancel(); local.Close(); remote.Close(); release() })
	<-done
}
