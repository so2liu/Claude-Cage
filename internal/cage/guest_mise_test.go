package cage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func miseArchive(t *testing.T, member, content string) ([]byte, string) {
	t.Helper()
	var buffer bytes.Buffer
	z := gzip.NewWriter(&buffer)
	w := tar.NewWriter(z)
	if err := w.WriteHeader(&tar.Header{Name: member, Mode: 0755, Size: int64(len(content))}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(content)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := z.Close(); err != nil {
		t.Fatal(err)
	}
	data := buffer.Bytes()
	return data, fmt.Sprintf("%x", sha256.Sum256(data))
}

func TestMiseArchivesHaveIndependentPinsResumeAndFallback(t *testing.T) {
	for _, tc := range []struct{ arch, mode, sources string }{
		{"amd64", "fresh", "release:0"}, {"arm64", "fresh", "release:0"},
		{"amd64", "official-failure", "release:0 npm:0"},
		{"amd64", "bad-mirror", "release:0 npm:0 release:0"},
		{"arm64", "cached-release", ""}, {"amd64", "partial-release", "release:7"}, {"arm64", "partial-npm", "release:0 npm:7"},
		{"amd64", "both-corrupt", "release:0 npm:0 release:0"},
	} {
		t.Run(tc.arch+"/"+tc.mode, func(t *testing.T) {
			dir := t.TempDir()
			binary := "#!/bin/sh\necho 2026.10.25\n# " + tc.arch + "\n"
			npm, nh := miseArchive(t, "package/bin/mise", binary)
			release, rh := miseArchive(t, "mise/bin/mise", binary)
			cache := filepath.Join(dir, ".cache", "cage", "downloads", rh)
			write := func(path string, data []byte, mode os.FileMode) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, data, mode); err != nil {
					t.Fatal(err)
				}
			}
			write(filepath.Join(dir, "npm-input"), npm, 0600)
			write(filepath.Join(dir, "release-input"), release, 0600)
			switch tc.mode {
			case "cached-release":
				write(filepath.Join(cache, "mise.tar.gz"), release, 0600)
			case "partial-release":
				write(filepath.Join(cache, "mise.tar.gz.part"), release[:7], 0600)
			case "partial-npm":
				write(filepath.Join(cache, nh, "mise.tar.gz.part"), npm[:7], 0600)
			}
			if _, err := exec.LookPath("shasum"); err == nil {
				write(filepath.Join(dir, "sha256sum"), []byte("#!/bin/sh\nexec shasum -a 256 \"$@\"\n"), 0755)
			} else if _, err = exec.LookPath("sha256sum"); err != nil {
				t.Skip("SHA-256 command unavailable")
			}
			for name, cmd := range map[string]string{
				"flock": "exit 0",
				"dpkg":  "if [ \"$1\" = --print-architecture ]; then echo \"$TEST_ARCH\"; else test \"$2\" = \"$4\"; fi",
				"curl": `set -eu
for arg in "$@"; do [ "$arg" != --head ] || exit 0; done
while [ "$#" -gt 0 ]; do
  case "$1" in
   "https://cdn.npmmirror.com/packages/%40jdxcode/mise-linux-$NPM_ARCH/2026.10.25/mise-linux-$NPM_ARCH-2026.10.25.tgz") src=npm ;;
   "https://github.com/jdx/mise/releases/download/v2026.10.25/mise-v2026.10.25-linux-$NPM_ARCH.tar.gz") src=release ;;
   -o) shift; out=$1 ;;
  esac
  shift
done
n=0
if [ -f "$out" ]; then n=$(wc -c <"$out" | tr -d ' '); fi
printf '%s:%s\n' "$src" "$n" >> "$HOME/sources"
if [ "$src" = release ]; then
  case "$TEST_MODE" in official-failure|partial-npm) printf 503; exit 22;; esac
  if [ "$TEST_MODE" = bad-mirror ] && [ "$(wc -l < "$HOME/sources" | tr -d ' ')" = 1 ]; then printf 503; exit 22; fi
fi
if [ "$TEST_MODE" = both-corrupt ] || { [ "$src" = npm ] && [ "$TEST_MODE" = bad-mirror ]; }; then
  printf wrong-archive >"$out"
else
  dd if="$HOME/$src-input" bs=1 skip="$n" 2>/dev/null >>"$out"
fi
printf 200`,
			} {
				write(filepath.Join(dir, name), []byte("#!/bin/sh\n"+cmd+"\n"), 0755)
			}
			npmArch := tc.arch
			if npmArch == "amd64" {
				npmArch = "x64"
			}
			script := ToolInstallPrelude() + fmt.Sprintf("\nCAGE_MISE_%s_SHA256=%s\nCAGE_MISE_NPM_%s_SHA256=%s\n", strings.ToUpper(tc.arch), rh, strings.ToUpper(tc.arch), nh) + "cage_install_tool mise"
			run := func() ([]byte, error) {
				cmd := exec.CommandContext(t.Context(), "bash", "-c", script)
				cmd.Env = append(os.Environ(), "HOME="+dir, "PATH="+dir+":"+os.Getenv("PATH"), "NPM_ARCH="+npmArch, "TEST_ARCH="+tc.arch, "TEST_MODE="+tc.mode, "CAGE_SETUP_PROGRESS=1")
				return cmd.CombinedOutput()
			}
			out, err := run()
			got, fileErr := os.ReadFile(filepath.Join(dir, ".local", "bin", "mise"))
			if tc.mode == "both-corrupt" {
				if err == nil || !os.IsNotExist(fileErr) {
					t.Fatalf("corrupted tool installed: %s", out)
				}
			} else {
				if err != nil || fileErr != nil || string(got) != binary {
					t.Fatalf("mise installation: %v %v %s", err, fileErr, out)
				}
				if tc.sources != "" && !strings.Contains(string(out), "CAGE_TOOL_BYTES mise.tar.gz ") {
					t.Fatalf("missing byte progress: %s", out)
				}
				if out, err = run(); err != nil || !strings.Contains(string(out), "already meets") {
					t.Fatalf("installed version was not reused: %v %s", err, out)
				}
			}
			sources, _ := os.ReadFile(filepath.Join(dir, "sources"))
			if got := strings.Join(strings.Fields(string(sources)), " "); got != tc.sources {
				t.Fatalf("sources %q want %q", got, tc.sources)
			}
		})
	}
}
