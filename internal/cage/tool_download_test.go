package cage

import (
	"bytes"
	"crypto/sha256"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func parallelToolFixture(t *testing.T, server *httptest.Server) (string, []string) {
	t.Helper()
	for _, name := range []string{"curl", "python3", "bash", "shasum"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skip(name + " unavailable")
		}
	}
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sha256sum"), []byte("#!/bin/sh\nexec shasum -a 256 \"$@\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	return dir, append(os.Environ(), "HOME="+dir, "PATH="+dir+":"+os.Getenv("PATH"), "CURL_CA_BUNDLE="+ca, "NO_PROXY=127.0.0.1", "no_proxy=127.0.0.1", "CAGE_SETUP_PROGRESS=1")
}

func toolFixtureBytes() []byte {
	data := make([]byte, 8<<20+137)
	for i := range data {
		data[i] = byte((i/8191 + i) % 251)
	}
	return data
}

func TestToolParallelDownloadFallbackAndHash(t *testing.T) {
	data := toolFixtureBytes()
	for _, mode := range []string{"parallel", "ignored", "wrong-range", "corrupt"} {
		t.Run(mode, func(t *testing.T) {
			var active, peak, bounded, gets atomic.Int32
			ready := make(chan struct{})
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/release" {
					http.Redirect(w, r, "/asset", http.StatusFound)
					return
				}
				if r.Method == http.MethodHead {
					w.Header().Set("Content-Length", fmt.Sprint(len(data)))
					return
				}
				gets.Add(1)
				head := r.Header.Get("Range")
				if head != "" && !strings.HasSuffix(head, "-") {
					now := active.Add(1)
					defer active.Add(-1)
					for old := peak.Load(); now > old && !peak.CompareAndSwap(old, now); old = peak.Load() {
					}
					if bounded.Add(1) == 4 {
						close(ready)
					}
					select {
					case <-ready:
					case <-r.Context().Done():
						return
					case <-time.After(3 * time.Second):
						t.Error("ranges not concurrent")
						return
					}
					if mode == "ignored" {
						r.Header.Del("Range")
					}
					if mode == "wrong-range" {
						w.Header().Set("Content-Range", "bytes 0-1/2")
						w.WriteHeader(206)
						io.WriteString(w, "xx")
						return
					}
				}
				body := data
				if mode == "corrupt" {
					body = bytes.Repeat([]byte{'x'}, len(data))
				}
				http.ServeContent(w, r, "package", time.Time{}, bytes.NewReader(body))
			}))
			defer server.Close()
			dir, env := parallelToolFixture(t, server)
			hash := fmt.Sprintf("%x", sha256.Sum256(data))
			script := "set -euo pipefail\n" + ToolInstallPrelude() + `cage_download_tool "$DOWNLOAD_URL" "$DOWNLOAD_HASH" "$HOME/package"
cage_download_tool "$DOWNLOAD_URL" "$DOWNLOAD_HASH" "$HOME/package"`
			command := exec.CommandContext(t.Context(), "bash", "-c", script)
			command.Env = append(env, "DOWNLOAD_URL="+server.URL+"/release", "DOWNLOAD_HASH="+hash)
			output, err := command.CombinedOutput()
			if mode == "corrupt" {
				if err == nil || !strings.Contains(string(output), "checksum mismatch") {
					t.Fatalf("bad hash accepted: %v %s", err, output)
				}
				if _, err := os.Stat(filepath.Join(dir, "package")); !os.IsNotExist(err) {
					t.Fatal("corrupt package published")
				}
			} else {
				if err != nil {
					t.Fatalf("%v %s", err, output)
				}
				got, err := os.ReadFile(filepath.Join(dir, "package"))
				if err != nil || !bytes.Equal(got, data) {
					t.Fatalf("incorrect bytes %v", err)
				}
				if mode == "parallel" && gets.Load() != 4 {
					t.Fatalf("cache fetched again: gets=%d", gets.Load())
				}
				if _, err := os.Stat(filepath.Join(dir, "package.part.ranges")); !os.IsNotExist(err) {
					t.Fatal("range cache not cleaned")
				}
			}
			if peak.Load() != 4 {
				t.Fatalf("connections=%d output=%s", peak.Load(), output)
			}
		})
	}
}

func TestToolParallelCancellationAndResume(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Linux tool script cancellation uses POSIX signals")
	}
	data := toolFixtureBytes()
	var interrupted atomic.Bool
	interrupted.Store(true)
	var calls atomic.Int32
	var resumed atomic.Bool
	ready := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", fmt.Sprint(len(data)))
			return
		}
		var start, end int64
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end); err != nil {
			t.Error(err)
			return
		}
		if interrupted.Load() {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
			w.Header().Set("Content-Length", fmt.Sprint(end-start+1))
			w.WriteHeader(206)
			w.Write(data[start : start+4096])
			w.(http.Flusher).Flush()
			if calls.Add(1) == 4 {
				close(ready)
			}
			<-r.Context().Done()
			return
		}
		if start%int64((len(data)+3)/4) == 4096 {
			resumed.Store(true)
		}
		http.ServeContent(w, r, "package", time.Time{}, bytes.NewReader(data))
	}))
	defer server.Close()
	dir, env := parallelToolFixture(t, server)
	target := filepath.Join(dir, "package.part")
	run := func() *exec.Cmd {
		command := exec.CommandContext(t.Context(), "python3", "-c", toolDownload, target, "--proto", "=https", "--proto-redir", "=https", "--fail", "--location", "--connect-timeout", "15", "--continue-at", "-", "--write-out", "%{http_code}", server.URL, "-o", target)
		command.Env = env
		return command
	}
	first := run()
	var output bytes.Buffer
	first.Stdout, first.Stderr = &output, &output
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		first.Process.Kill()
		first.Wait()
		t.Fatal("no parallel requests")
	}
	time.Sleep(150 * time.Millisecond)
	if err := first.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- first.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled transfer succeeded")
		}
	case <-time.After(3 * time.Second):
		first.Process.Kill()
		t.Fatal("cancellation did not join workers")
	}
	info, err := os.Stat(target)
	if err != nil || info.Size() != 4096 {
		t.Fatalf("prefix=%v err=%v output=%s", info, err, &output)
	}
	interrupted.Store(false)
	if out, err := run().CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	got, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(got, data) || !resumed.Load() {
		t.Fatalf("resume=%v bytes=%v err=%v", resumed.Load(), bytes.Equal(got, data), err)
	}
}

func TestGuestServiceEntrypointsIncludeToolDownloader(t *testing.T) {
	for name, script := range map[string]string{"first-install": GuestSetup, "start-and-sync": DefaultConfig().StartScript()} {
		t.Run(name, func(t *testing.T) {
			_, functions, ok := strings.Cut(script, "\nCAGE_WAIT_SERVICES\n")
			if !ok {
				t.Fatal("service entry missing")
			}
			functions, _, ok = strings.Cut(functions, "sudo tee /usr/local/lib/tailnet-client.mjs")
			if !ok {
				t.Fatal("service function boundary missing")
			}
			dir := t.TempDir()
			curl := `#!/bin/sh
for arg in "$@"; do [ "$arg" != --head ] || exit 0; done
while [ "$#" -gt 0 ]; do
 if [ "$1" = -o ]; then shift; printf fixture > "$1"; printf 200; exit 0; fi
 shift
done
exit 99
`
			for file, body := range map[string]string{"curl": curl, "sha256sum": "#!/bin/sh\nexec shasum -a 256 \"$@\"\n"} {
				if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0755); err != nil {
					t.Fatal(err)
				}
			}
			checksum := fmt.Sprintf("%x", sha256.Sum256([]byte("fixture")))
			command := exec.CommandContext(t.Context(), "bash", "-c", "set -euo pipefail\n"+functions+`cage_download_tool https://example.invalid/archive "$CHECKSUM" "$HOME/package"`)
			command.Env = append(os.Environ(), "HOME="+dir, "PATH="+dir+":"+os.Getenv("PATH"), "CHECKSUM="+checksum)
			if out, err := command.CombinedOutput(); err != nil {
				t.Fatalf("service download missing: %v %s", err, out)
			}
			got, err := os.ReadFile(filepath.Join(dir, "package"))
			if err != nil || string(got) != "fixture" {
				t.Fatalf("service package %q %v", got, err)
			}
		})
	}
}
