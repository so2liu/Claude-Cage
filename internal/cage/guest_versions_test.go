package cage

import (
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGuestManifestIsPublishedOnlyAfterSuccessfulServiceStart(t *testing.T) {
	for _, body := range []string{"printf started; exit 0", "printf broken; exit 7"} {
		t.Run(body, func(t *testing.T) {
			dir := t.TempDir()
			marker := filepath.Join(dir, "published")
			// Observe privileged actions without touching the host's /etc or running services.
			sudo := `#!/bin/sh
case "$1" in
 rm) rm -f "$AUDIT_MARKER";;
 python3) cat >/dev/null; printf published > "$AUDIT_MARKER";;
 *) exit 91;;
esac
`
			if err := os.WriteFile(filepath.Join(dir, "sudo"), []byte(sudo), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(marker, []byte("old"), 0600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command("bash", "-c", markGuestStart(body))
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "AUDIT_MARKER="+marker)
			_, err := cmd.CombinedOutput()
			data, readErr := os.ReadFile(marker)
			if strings.Contains(body, "exit 0") {
				if err != nil || readErr != nil || string(data) != "published" {
					t.Fatal("successful startup did not publish manifest", err, readErr)
				}
			} else if err == nil || !os.IsNotExist(readErr) {
				t.Fatal("failed startup retained or published a manifest", err, readErr)
			}
		})
	}
}

func TestToolDownloadWithRealCurlAndTLS(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl unavailable")
	}
	dir := t.TempDir()
	if _, err := exec.LookPath("sha256sum"); err != nil || runtime.GOOS == "darwin" {
		if _, err := exec.LookPath("shasum"); err != nil {
			t.Skip("SHA-256 tool unavailable")
		}
		if err := os.WriteFile(filepath.Join(dir, "sha256sum"), []byte("#!/bin/sh\nexec shasum -a 256 \"$@\"\n"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	data := "complete download bytes"
	var calls atomic.Int32
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", fmt.Sprint(len(data)))
			return
		}
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Length", fmt.Sprint(len(data)))
			fmt.Fprint(w, data[:5])
			return
		}
		if r.Header.Get("Range") != "bytes=5-" {
			t.Errorf("range: %q", r.Header.Get("Range"))
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 5-%d/%d", len(data)-1, len(data)))
		w.WriteHeader(206)
		fmt.Fprint(w, data[5:])
	}))
	defer s.Close()
	ca := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(data))
	cmd := exec.Command("bash", "-c", "set -euo pipefail\n"+ToolInstallPrelude()+`cage_download_tool "$DOWNLOAD_URL" "$DOWNLOAD_HASH" "$HOME/package"
cage_download_tool "$DOWNLOAD_URL" "$DOWNLOAD_HASH" "$HOME/package"`)
	cmd.Env = append(os.Environ(), "HOME="+dir, "PATH="+dir+":"+os.Getenv("PATH"), "CURL_CA_BUNDLE="+ca, "DOWNLOAD_URL="+s.URL, fmt.Sprintf("DOWNLOAD_HASH=%x", hash), "NO_PROXY=127.0.0.1", "no_proxy=127.0.0.1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	got, err := os.ReadFile(filepath.Join(dir, "package"))
	if err != nil || string(got) != data || calls.Load() != 2 {
		t.Fatalf("%q %v calls=%d", got, err, calls.Load())
	}
}

func TestToolDownloadIdleTimeoutAndManualResume(t *testing.T) {
	for _, tool := range []string{"curl", "python3", "bash", "shasum"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " unavailable")
		}
	}
	for _, mode := range []string{"slow", "stalled", "no-body"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			for name, body := range map[string]string{
				"sha256sum": "#!/bin/sh\nexec shasum -a 256 \"$@\"\n",
				"sleep":     "#!/bin/sh\nexit 0\n",
			} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0755); err != nil {
					t.Fatal(err)
				}
			}
			const data = "abcdefghijklmnop"
			var calls atomic.Int32
			var recovered atomic.Bool
			s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodHead {
					w.Header().Set("Content-Length", fmt.Sprint(len(data)))
					return
				}
				call := calls.Add(1)
				offset := 0
				if mode == "stalled" {
					offset = int(call - 1)
				}
				wantRange := ""
				if offset > 0 {
					wantRange = fmt.Sprintf("bytes=%d-", offset)
				}
				if r.Header.Get("Range") != wantRange {
					t.Errorf("range = %q, want %q", r.Header.Get("Range"), wantRange)
				}
				w.Header().Set("Content-Length", fmt.Sprint(len(data)-offset))
				if offset > 0 {
					w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, len(data)-1, len(data)))
					w.WriteHeader(http.StatusPartialContent)
				}
				if recovered.Load() {
					fmt.Fprint(w, data[offset:])
					return
				}
				if mode == "slow" {
					for i := range len(data) {
						fmt.Fprint(w, data[i:i+1])
						w.(http.Flusher).Flush()
						select {
						case <-r.Context().Done():
							return
						case <-time.After(100 * time.Millisecond):
						}
					}
					return
				}
				if mode == "stalled" {
					fmt.Fprint(w, data[offset:offset+1])
				}
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			defer s.Close()
			ca := filepath.Join(dir, "ca.pem")
			if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw}), 0600); err != nil {
				t.Fatal(err)
			}
			hash := sha256.Sum256([]byte(data))
			run := func() ([]byte, error) {
				// Shorten only the test's idle interval; use the real curl, TLS,
				// filesystem, checksum, automatic retries and manual continuation.
				prelude := strings.Replace(ToolInstallPrelude(), "idle_timeout = 60", "idle_timeout = 0.5", 1)
				cmd := exec.CommandContext(t.Context(), "bash", "-c", prelude+`cage_download_tool "$DOWNLOAD_URL" "$DOWNLOAD_HASH" "$HOME/package"`)
				cmd.Env = append(os.Environ(), "CAGE_SETUP_PROGRESS=1", "HOME="+dir, "PATH="+dir+":"+os.Getenv("PATH"), "CURL_CA_BUNDLE="+ca, "DOWNLOAD_URL="+s.URL, fmt.Sprintf("DOWNLOAD_HASH=%x", hash), "NO_PROXY=127.0.0.1", "no_proxy=127.0.0.1")
				return cmd.CombinedOutput()
			}
			out, err := run()
			if mode == "slow" {
				if !strings.Contains(string(out), "CAGE_TOOL_BYTES package 0") || !strings.Contains(string(out), "CAGE_TOOL_BYTES package 16") {
					t.Fatalf("real tool transfer did not publish its byte counts: %s", out)
				}
				if err != nil || calls.Load() != 1 {
					t.Fatalf("progressing download interrupted: %v calls=%d %s", err, calls.Load(), out)
				}
			} else {
				if err == nil || calls.Load() != 4 || !strings.Contains(string(out), "no data") {
					t.Fatalf("stalled download did not exhaust idle retries: %v calls=%d %s", err, calls.Load(), out)
				}
				if _, err := os.Stat(filepath.Join(dir, "package")); !os.IsNotExist(err) {
					t.Fatal("incomplete file published")
				}
				if mode == "stalled" {
					got, err := os.ReadFile(filepath.Join(dir, "package.part"))
					if err != nil || string(got) != data[:4] {
						t.Fatalf("partial data lost: %q %v", got, err)
					}
				}
				recovered.Store(true)
				if out, err := run(); err != nil || calls.Load() != 5 {
					t.Fatalf("manual continuation failed: %v calls=%d %s", err, calls.Load(), out)
				}
			}
			got, err := os.ReadFile(filepath.Join(dir, "package"))
			if err != nil || string(got) != data {
				t.Fatalf("download not verified and published: %q %v", got, err)
			}
		})
	}
}

func TestClaudeNativeCacheUsesGuestArchitectureAndIntegrity(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq unavailable")
	}
	for _, arch := range []string{"arm64", "amd64"} {
		t.Run(arch, func(t *testing.T) {
			dir := t.TempDir()
			hash := sha512.Sum512([]byte("native-package"))
			packageName := "claude-code-linux-arm64"
			if arch == "amd64" {
				packageName = "claude-code-linux-x64"
			}
			commands := map[string]string{
				"dpkg":   "#!/bin/sh\nif [ \"$1\" = --print-architecture ]; then echo \"$TEST_ARCH\"; else exit 1; fi\n",
				"claude": "#!/bin/sh\necho 1.0.0\n",
				"flock":  "#!/bin/sh\nexit 0\n",
				"curl": `#!/bin/sh
for arg in "$@"; do [ "$arg" != --head ] || exit 0; done
while [ "$#" -gt 0 ]; do
  case "$1" in
    https://cdn.npmmirror.com/packages/%40anthropic-ai/"$TEST_PACKAGE"/9.8.7/"$TEST_PACKAGE"-9.8.7.tgz)
      body=native-package
      [ "$TEST_ARCH" != amd64 ] || body=corrupt-mirror
      echo mirror >> "$HOME/sources";;
    https://registry.npmjs.org/"$TEST_PACKAGE".tgz)
      body=native-package
      echo upstream >> "$HOME/sources";;
    -o) shift; printf %s "${body:?unexpected source}" > "$1"; printf 200; exit;;
  esac
  shift
done
exit 1
`,
				"npm": `#!/bin/sh
case "$1" in
view)
  if [ "$3" = @anthropic-ai/claude-code@latest ]; then
    test "$4 $5" = 'version --json' || exit 96
    printf '"9.8.7"'; exit
  fi
  test "$3" = "@anthropic-ai/$TEST_PACKAGE@9.8.7" || exit 91
  printf '{"integrity":"sha512-%s","tarball":"https://registry.npmjs.org/%s.tgz"}' "$TEST_INTEGRITY" "$TEST_PACKAGE";;
cache)
  test "$2 $3 $4" = 'add --offline --ignore-scripts' || exit 92
  test "$(cat "$5")" = native-package || exit 93
  printf cached > "$HOME/cached";;
install)
  test -f "$HOME/cached" || exit 94
  test "$6" = '@anthropic-ai/claude-code@9.8.7' || exit 97
  printf installed > "$HOME/installed";;
*) exit 95;;
esac
`,
			}
			if _, err := exec.LookPath("sha512sum"); err != nil || runtime.GOOS == "darwin" {
				if _, err := exec.LookPath("shasum"); err != nil {
					t.Skip("SHA-512 tool unavailable")
				}
				commands["sha512sum"] = "#!/bin/sh\nexec shasum -a 512 \"$@\"\n"
			}
			for name, body := range commands {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0755); err != nil {
					t.Fatal(err)
				}
			}
			// The simulated amd64 guest may be running on an arm64 test host.
			cpuinfo := filepath.Join(dir, "cpuinfo")
			if err := os.WriteFile(cpuinfo, []byte("flags\t: ssse3 sse4_1 sse4_2 popcnt\n"), 0600); err != nil {
				t.Fatal(err)
			}
			prelude := strings.ReplaceAll(ToolInstallPrelude(), "/proc/cpuinfo", `"$HOME/cpuinfo"`)
			cmd := exec.Command("bash", "-c", prelude+"cage_install_tool claude")
			cmd.Env = append(os.Environ(), "HOME="+dir, "PATH="+dir+":"+os.Getenv("PATH"), "TEST_ARCH="+arch, "TEST_PACKAGE="+packageName, "TEST_INTEGRITY="+base64.StdEncoding.EncodeToString(hash[:]))
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%v %s", err, out)
			}
			if _, err := os.Stat(filepath.Join(dir, "installed")); err != nil {
				t.Fatal(err)
			}
			want := "upstream\n"
			if got, err := os.ReadFile(filepath.Join(dir, "sources")); err != nil || string(got) != want {
				t.Fatalf("sources: %q, want %q (%v)", got, want, err)
			}
			if arch == "amd64" {
				if err := os.WriteFile(cpuinfo, []byte("flags\t: sse2 sse4a popcnt\n"), 0600); err != nil {
					t.Fatal(err)
				}
				retry := exec.Command("bash", "-c", prelude+"cage_install_tool claude")
				retry.Env = cmd.Env
				if out, err := retry.CombinedOutput(); err == nil || !strings.Contains(string(out), "需要 SSE4.2") {
					t.Fatalf("missing guest CPU feature should stop Claude setup: %v %s", err, out)
				}
			}
		})
	}
}

func TestClaudeLatestResolutionPreservesNewerInstallAndRejectsBadMetadata(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq unavailable")
	}
	for _, metadata := range []string{`"9.8.7"`, `"latest"`, `{}`, `"9.8.7;touch bad"`, `null`} {
		t.Run(metadata, func(t *testing.T) {
			dir := t.TempDir()
			// Exercise the real installation entry point. Resolving latest is
			// mandatory even when an existing install satisfies the old floor.
			cmd := exec.Command("bash", "-c", ToolInstallPrelude()+`
dpkg() {
  if [ "$1" = --print-architecture ]; then echo arm64; return; fi
  test "$*" = '--compare-versions 9.9.9 ge 9.8.7'
}
claude() { echo '9.9.9 (Claude Code)'; }
npm() {
  printf '%s\n' "$*" >> "$HOME/npm-calls"
  test "$*" = 'view --registry=https://registry.npmjs.org @anthropic-ai/claude-code@latest version --json' || return 98
  printf '%s' "$TEST_METADATA"
}
cage_install_tool claude`)
			cmd.Env = append(os.Environ(), "HOME="+dir, "TEST_METADATA="+metadata)
			out, err := cmd.CombinedOutput()
			calls, readErr := os.ReadFile(filepath.Join(dir, "npm-calls"))
			if readErr != nil || string(calls) != "view --registry=https://registry.npmjs.org @anthropic-ai/claude-code@latest version --json\n" {
				t.Fatalf("resolution reached cache/install or repeated the lookup: %q %v", calls, readErr)
			}
			if metadata == `"9.8.7"` {
				if err != nil || !strings.Contains(string(out), "already meets required version 9.8.7") {
					t.Fatalf("newer install changed or latest not resolved: %v %s", err, out)
				}
			} else if err == nil {
				t.Fatalf("bad metadata accepted: %s", out)
			}
		})
	}
}

func TestGeneratedGuestScriptsParseAsBash(t *testing.T) {
	for name, script := range map[string]string{"start": DefaultConfig().StartScript(), "tools": ToolsUpdateScript(), "first-install-functions": ToolInstallPrelude()} {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command("bash", "-n")
			cmd.Stdin = strings.NewReader(script)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%v: %s", err, out)
			}
		})
	}
}

func TestChromiumPrefetchFallsBackButPropagatesCancellation(t *testing.T) {
	for _, status := range []int{1, 143} {
		cmd := exec.Command("bash", "-c", fmt.Sprintf("set -e\nsudo() { cat >/dev/null; return %d; }\n", status)+ToolInstallPrelude()+"cage_cache_chromium --only-upgrade chromium\necho normal-apt")
		out, err := cmd.CombinedOutput()
		fellBack := err == nil && strings.Contains(string(out), "normal-apt")
		if (status == 1 && !fellBack) || (status == 143 && (err == nil || cmd.ProcessState.ExitCode() != 143)) {
			t.Fatalf("prefetch exit %d: %v %s", status, err, out)
		}
	}
}

func TestPinnedToolInstallerRejectsWrongHashBeforeInstalling(t *testing.T) {
	dir := t.TempDir()
	commands := map[string]string{
		"flock":       "#!/bin/sh\nexit 0\n",
		"dpkg":        "#!/bin/sh\nif [ \"$1\" = --print-architecture ]; then echo arm64; else exit 1; fi\n",
		"code-server": "#!/bin/sh\necho 1.0.0\n",
		"curl":        "#!/bin/sh\nwhile [ \"$#\" -gt 0 ]; do if [ \"$1\" = -o ]; then shift; printf altered-package > \"$1\"; exit; fi; shift; done\nexit 1\n",
		"sha256sum":   "#!/bin/sh\ncat >/dev/null\nexit 1\n",
		"sudo":        "#!/bin/sh\nprintf attempted > \"$AUDIT_INSTALL\"\nexit 0\n",
	}
	for name, content := range commands {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0755); err != nil {
			t.Fatal(err)
		}
	}
	installed := filepath.Join(dir, "installed")
	cmd := exec.Command("bash", "-c", ToolInstallPrelude()+"\ncage_install_tool code-server")
	cmd.Env = append(os.Environ(), "HOME="+dir, "PATH="+dir+":"+os.Getenv("PATH"), "AUDIT_INSTALL="+installed)
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "checksum mismatch") {
		t.Fatalf("changed download accepted: %v %s", err, out)
	}
	if _, err := os.Stat(installed); !os.IsNotExist(err) {
		t.Fatal("installer ran before verifying the artifact")
	}
}

func TestPinnedToolVersionIgnoresFirstRunNotices(t *testing.T) {
	for _, version := range []string{"4.144.0", "v4.144.1", "4.144.0-rc.1"} {
		t.Run(version, func(t *testing.T) {
			dir := t.TempDir()
			commands := map[string]string{
				"code-server": "#!/bin/sh\nprintf '%s\\n' '[2026-11-05T14:58:49.600Z] info Wrote default config file' '" + version + " commit with Code 1.144.0'\n",
				"dpkg":        "#!/bin/sh\nif [ \"$1\" = --print-architecture ]; then echo arm64; else test \"$2\" = \"${EXPECTED_VERSION#v}\"; fi\n",
				"curl":        "#!/bin/sh\necho unexpected-download >&2\nexit 90\n",
			}
			for name, body := range commands {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0755); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command("bash", "-c", ToolInstallPrelude()+"\ncage_install_tool code-server")
			cmd.Env = append(os.Environ(), "HOME="+dir, "PATH="+dir+":"+os.Getenv("PATH"), "EXPECTED_VERSION="+version)
			out, err := cmd.CombinedOutput()
			if err != nil || !strings.Contains(string(out), "already meets") {
				t.Fatalf("%v: %s", err, out)
			}
		})
	}
}

func TestToolDownloadKeepsPartialAndResumes(t *testing.T) {
	dir := t.TempDir()
	commands := map[string]string{
		"sleep": "#!/bin/sh\nexit 0\n",
		"curl": `#!/bin/sh
while [ "$#" -gt 0 ]; do
  case "$1" in -o) shift; artifact="$1";; --continue-at) shift; test "$1" = - || exit 99;; esac
  shift
done
if [ ! -f "$HOME/recover" ]; then
  printf partial > "$artifact"
  exit 56
fi
test "$(cat "$artifact")" = partial || exit 98
printf complete > "$artifact"
printf 206
`,
		"sha256sum": "#!/bin/sh\nread checksum artifact\ntest \"$(cat \"$artifact\")\" = complete\n",
	}
	for name, body := range commands {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0755); err != nil {
			t.Fatal(err)
		}
	}
	run := func() ([]byte, error) {
		cmd := exec.Command("bash", "-c", ToolInstallPrelude()+`cage_download_tool https://example.invalid/tool pinned "$HOME/package"`)
		cmd.Env = append(os.Environ(), "HOME="+dir, "PATH="+dir+":"+os.Getenv("PATH"))
		return cmd.CombinedOutput()
	}
	if out, err := run(); err == nil || !strings.Contains(string(out), "partial data is saved") {
		t.Fatalf("%v %s", err, out)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "package.part")); err != nil || string(data) != "partial" {
		t.Fatal("partial lost", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "recover"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := run(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "package")); err != nil || string(data) != "complete" {
		t.Fatal("not published", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "package.part")); !os.IsNotExist(err) {
		t.Fatal("partial left after success")
	}
}
