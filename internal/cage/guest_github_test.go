package cage

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestGitHubCLIInstallAndExistingVersion(t *testing.T) {
	for _, arch := range []string{"arm64", "amd64"} {
		for _, installed := range []bool{false, true} {
			t.Run(arch+map[bool]string{false: "/missing", true: "/installed"}[installed], func(t *testing.T) {
				dir := t.TempDir()
				script := "set -euo pipefail\n" + ToolInstallPrelude() + `
dpkg() {
  if [ "$1" = --print-architecture ]; then printf '%s\n' "$TEST_ARCH";
  else test "$2" = "$4"; fi
}
gh() { if [ "$TEST_INSTALLED" = true ]; then printf 'gh version %s (2026-10-26)\nhttps://github.com/cli/cli\n' "$CAGE_GH_VERSION"; else return 127; fi; }
flock() { :; }
cage_download_tool() {
  test "$1" = "https://github.com/cli/cli/releases/download/v$CAGE_GH_VERSION/gh_${CAGE_GH_VERSION}_linux_${TEST_ARCH}.deb"
  if [ "$TEST_ARCH" = arm64 ]; then test "$2" = "$CAGE_GH_ARM64_SHA256"; else test "$2" = "$CAGE_GH_AMD64_SHA256"; fi
  printf package > "$3"
}
sudo() {
  test "$1" = env
  for arg in "$@"; do last=$arg; done
  test -f "$last"
  test "$(cat "$last")" = package
  printf installed > "$HOME/result"
}
cage_install_tool gh
`
				cmd := exec.Command("bash", "-c", script)
				cmd.Env = append(os.Environ(), "HOME="+dir, "TEST_ARCH="+arch, "TEST_INSTALLED="+map[bool]string{true: "true", false: "false"}[installed])
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("%v: %s", err, out)
				}
				_, err := os.Stat(filepath.Join(dir, "result"))
				if installed && !os.IsNotExist(err) || !installed && err != nil {
					t.Fatalf("unexpected package installation: installed=%v err=%v", installed, err)
				}
			})
		}
	}
}
