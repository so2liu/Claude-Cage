package workbench

import (
	"archive/tar"
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/clipboardfiles"
	"github.com/sylvinus/agent-vm/internal/guestos"
)

func TestLinuxClipboardCommandFitsWindowsProcessLimit(t *testing.T) {
	server := testServer(t)
	command, _, err := server.clipboardCommand(t.Context(), "stream", clipboardFileSelection{})
	if err != nil {
		t.Fatal(err)
	}
	// 给 SSH 路径、选项和 Windows 参数转义留足空间，防止脚本增长后重现启动失败。
	if len(strings.Join(command.Args, " ")) > 24<<10 {
		t.Fatal("Linux 客机剪贴板命令过长，可能超过 Windows 进程限制")
	}

	// 校验实际命令里的载荷，确保运行的是随当前后端发布的完整脚本。
	if len(command.Args) < 2 {
		t.Fatal("剪贴板启动命令缺少载荷")
	}
	fields := strings.Fields(command.Args[len(command.Args)-1])
	if len(fields) == 0 {
		t.Fatal("剪贴板启动命令缺少载荷")
	}
	compressed, err := base64.StdEncoding.DecodeString(fields[len(fields)-1])
	if err != nil {
		t.Fatal(err)
	}
	reader, err := zlib.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	script, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(script) != cage.SharedClipboardHelper {
		t.Fatal("剪贴板脚本在压缩传输中发生变化")
	}
}

func TestClipboardFileReleaseWaitsForArchiveReader(t *testing.T) {
	s := testServer(t)
	var released [][]string
	s.clipboardRelease = func(paths []string) { released = append(released, append([]string(nil), paths...)) }
	id := s.registerClipboardFiles([]string{"/created/fixture"}, "guest")
	selection, finish := s.acquireClipboardFiles(id)
	if len(selection.Paths) != 1 {
		t.Fatal("selection was not acquired")
	}
	s.releaseClipboardFiles(id)
	if len(s.clipboardFileSelection(id).Paths) != 0 || len(released) != 0 {
		t.Fatal("retirement interrupted an existing reader")
	}
	finish()
	if len(released) != 1 || released[0][0] != "/created/fixture" {
		t.Fatal("last reader did not release the guest copy")
	}
	s.releaseClipboardFiles(id)
	if len(released) != 1 {
		t.Fatal("selection was released twice")
	}
}

func TestClipboardFileReleaseSupportsLegacyAgents(t *testing.T) {
	s := testServer(t)
	id := s.registerClipboardFiles([]string{"/legacy/fixture"}, "guest")
	s.releaseClipboardFiles(id)
	if len(s.clipboardFileSelection(id).Paths) != 0 {
		t.Fatal("legacy selection reference remained")
	}
}

func TestClipboardCopyRejectsMissingRevisionAndInvalidPhaseBeforeGuestAccess(t *testing.T) {
	s := testServer(t)
	s.macosGuest = &guestos.MacOSProfile{Instance: "mac", User: "guestdev"}
	for _, body := range []string{
		`{"phase":"unknown"}`,
		`{"phase":"read","session":"gui-a"}`,
		`{"phase":"read","session":"gui-a","revision":-1}`,
		`{"phase":"read","session":"","revision":1}`,
	} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/api/clipboard/copy", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		s.clipboardCopy(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid copy request reached the guest: %d", response.Code)
		}
	}
}

func TestClipboardReceiveRequiresProducerCompletion(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "content")
	if err := os.WriteFile(path, []byte("complete bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := writeClipboardArchive(&archive, []string{path}); err != nil {
		t.Fatal(err)
	}
	for _, status := range []string{"ok", "failed", ""} {
		t.Run(status, func(t *testing.T) {
			temp := t.TempDir()
			t.Setenv("TMPDIR", temp)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Trailer", "X-Cage-Clipboard-Status")
				_, _ = w.Write(archive.Bytes())
				w.Header().Set("X-Cage-Clipboard-Status", status)
			}))
			defer server.Close()
			response, err := server.Client().Get(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			cache := clipboardfiles.CacheStore{Directory: filepath.Join(t.TempDir(), "records")}
			selection, err := receiveClipboardArchive(response, cache)
			paths := selection.Paths
			if status == "ok" {
				if err != nil || len(paths) != 1 {
					t.Fatalf("completed transfer failed: %v", err)
				}
				defer cache.Release(selection.ID)
				data, _ := os.ReadFile(paths[0])
				if string(data) != "complete bytes" {
					t.Fatal("file content changed")
				}
			} else {
				if err == nil || len(paths) != 0 {
					t.Fatal("failed guest published paths")
				}
				entries, _ := os.ReadDir(temp)
				if len(entries) != 0 {
					t.Fatal("failed transfer retained temporary files")
				}
			}
		})
	}
	// Losing the connection before all file bytes arrive must not create a file.
	response := &http.Response{Body: io.NopCloser(bytes.NewReader(archive.Bytes()[:520])), Trailer: http.Header{"X-Cage-Clipboard-Status": {"ok"}}}
	cache := clipboardfiles.CacheStore{Directory: filepath.Join(t.TempDir(), "records")}
	if selection, err := receiveClipboardArchive(response, cache); err == nil || len(selection.Paths) != 0 {
		t.Fatal("truncated archive succeeded")
	}
}

func TestSharedClipboardArchivePreservesFilesFoldersAndBytes(t *testing.T) {
	root := t.TempDir()
	folder := filepath.Join(root, "资料")
	if err := os.MkdirAll(filepath.Join(folder, "empty"), 0700); err != nil {
		t.Fatal(err)
	}
	data := []byte{0, 0xff, 0x80, '\n'}
	if err := os.WriteFile(filepath.Join(folder, "中文.bin"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "blank.txt"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	var wire bytes.Buffer
	if err := writeClipboardArchive(&wire, []string{folder, filepath.Join(root, "blank.txt")}); err != nil {
		t.Fatal(err)
	}
	paths, err := readClipboardArchive(&wire)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(filepath.Dir(paths[0]))
	actual, err := os.ReadFile(filepath.Join(paths[0], "中文.bin"))
	if err != nil || !bytes.Equal(actual, data) {
		t.Fatalf("binary changed: %v", err)
	}
	if info, err := os.Stat(filepath.Join(paths[0], "empty")); err != nil || !info.IsDir() {
		t.Fatal("empty folder lost")
	}
	if info, err := os.Stat(paths[1]); err != nil || info.Size() != 0 {
		t.Fatal("empty file lost")
	}
}

func TestSharedClipboardRequiresSessionAndUnlockedAccount(t *testing.T) {
	s := testServer(t)
	for _, endpoint := range []string{"/api/clipboard", "/api/clipboard/files", "/api/clipboard/files/10000000-2222-4333-8444-123456789abc"} {
		method := "GET"
		if endpoint == "/api/clipboard/files" {
			method = "POST"
		}
		if response := request(s, method, endpoint, s.origin, "", false); response.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated %s: %d", endpoint, response.Code)
		}
	}
	s.account.lock()
	if response := request(s, "GET", "/api/clipboard", s.origin, "", true); response.Code != http.StatusLocked {
		t.Fatalf("locked clipboard: %d", response.Code)
	}
}

// Opt-in integration: uses disposable real X11 displays in the supplied QA VM.
func TestSharedClipboardGuestWebSocketAndFileStream(t *testing.T) {
	config := os.Getenv("CAGE_CLIPBOARD_QA_SSH_CONFIG")
	if config == "" {
		t.Skip("isolated clipboard guest not configured")
	}
	s := testServer(t)
	s.ready = true
	if err := os.MkdirAll(s.instanceDir(), 0700); err != nil {
		t.Fatal(err)
	}
	ssh, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	// Replace only the QA Host selector; all credentials remain in its file.
	lines := strings.Split(string(ssh), "\n")
	for index, line := range lines {
		if strings.HasPrefix(line, "Host ") {
			lines[index] = "Host *"
		}
	}
	if err := os.WriteFile(filepath.Join(s.instanceDir(), "ssh.config"), []byte(strings.Join(lines, "\n")), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.instanceDir(), "cage-running.json"), []byte(fmt.Sprintf(`{"pid":%d}`, os.Getpid())), 0600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(s.handler())
	defer server.Close()
	s.origin = server.URL
	headers := http.Header{"Origin": {s.origin}, "Cookie": {s.cookieName() + "=" + s.session}}
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/api/clipboard", headers)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	_ = ws.SetReadDeadline(time.Now().Add(12 * time.Second))
	if err := ws.WriteJSON(map[string]any{"text": "production wire 中文🙂", "serial": 1}); err != nil {
		t.Fatal(err)
	}
	for {
		var event map[string]any
		if err := ws.ReadJSON(&event); err != nil {
			t.Fatal(err)
		}
		if event["ack"] == float64(1) {
			break
		}
	}
	root := t.TempDir()
	folder := filepath.Join(root, "复制目录")
	if err := os.Mkdir(folder, 0700); err != nil {
		t.Fatal(err)
	}
	data := []byte{0, 255, 128, 1, 2, 3}
	if err := os.WriteFile(filepath.Join(folder, "binary.bin"), data, 0600); err != nil {
		t.Fatal(err)
	}
	var archive bytes.Buffer
	if err := writeClipboardArchive(&archive, []string{folder}); err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest("POST", s.origin+"/api/clipboard/files", &archive)
	request.Header = headers.Clone()
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var uploaded struct {
		Files string `json:"files"`
	}
	decodeErr := json.NewDecoder(response.Body).Decode(&uploaded)
	response.Body.Close()
	if response.StatusCode != 200 || decodeErr != nil || uploaded.Files == "" {
		t.Fatalf("file upload: %d %v", response.StatusCode, decodeErr)
	}
	if err := ws.WriteJSON(map[string]any{"files": uploaded.Files, "serial": 2}); err != nil {
		t.Fatal(err)
	}
	for {
		var event map[string]any
		if err := ws.ReadJSON(&event); err != nil {
			t.Fatal(err)
		}
		if event["ack"] == float64(2) {
			break
		}
	}
	request, _ = http.NewRequest("GET", s.origin+"/api/clipboard/files/"+uploaded.Files, nil)
	request.Header = headers.Clone()
	response, err = server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := readClipboardArchive(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(filepath.Dir(paths[0]))
	actual, err := os.ReadFile(filepath.Join(paths[0], "binary.bin"))
	if err != nil || !bytes.Equal(actual, data) {
		t.Fatal("real guest stream changed file bytes")
	}
}

func TestSharedClipboardArchiveRejectsTraversalAndLinks(t *testing.T) {
	for _, header := range []tar.Header{
		{Name: "../escape", Typeflag: tar.TypeReg}, {Name: "/absolute", Typeflag: tar.TypeReg},
		{Name: "folder/../escape", Typeflag: tar.TypeReg}, {Name: "file:stream", Typeflag: tar.TypeReg},
		{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/secret"}, {Name: "hard", Typeflag: tar.TypeLink, Linkname: "other"},
		{Name: "large", Typeflag: tar.TypeReg, Size: browserFileLimit + 1},
	} {
		t.Run(header.Name, func(t *testing.T) {
			var wire bytes.Buffer
			writer := tar.NewWriter(&wire)
			if err := writer.WriteHeader(&header); err != nil {
				t.Fatal(err)
			}
			_ = writer.Close()
			if _, err := readClipboardArchive(&wire); err == nil {
				t.Fatal("invalid archive accepted")
			}
		})
	}
}

func TestSharedClipboardArchiveRejectsSelectedSymlinks(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "source"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "source"), filepath.Join(root, "link")); err != nil {
		t.Skip(err)
	}
	var wire bytes.Buffer
	if err := writeClipboardArchive(&wire, []string{filepath.Join(root, "link")}); err == nil {
		t.Fatal("selected symlink accepted")
	}
}
