package guestos

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/version"
)

const MacOSBrowserTools = "/usr/local/libexec/cage-workbench-browser-4.145.0-pw1.65.0"
const MacOSCLIRuntime = "/usr/local/libexec/cage-cli-runtime-2.1.311-py3.13.18-git2.56.0"

type NativeComponentFile struct {
	Source          string `json:"source"`
	Target          string `json:"target,omitempty"`
	SHA256          string `json:"sha256"`
	InstalledSHA256 string `json:"installed_sha256"`
}

type NativeComponentManifest struct {
	cage.GuestManifest
	System string                `json:"system"`
	Files  []NativeComponentFile `json:"files"`
	Tools  map[string]string     `json:"tools"`
}

// The installed application bundle is the required component source. Walking
// its actual contents includes newly added helper resources and browser modules.
func RequiredNativeComponents(kit, system string) (NativeComponentManifest, error) {
	manifest := NativeComponentManifest{GuestManifest: cage.GuestManifest{Protocol: 1, AppVersion: version.Version}, System: system}
	if system != MacOS && system != Windows11 {
		return manifest, errors.New("原生客机系统无效")
	}
	essential := []string{"payload/Cage.GuestClipboard.exe", "payload/cage-guest-files.exe", "payload/cage-guest-boot.exe", "payload/Cage.HyperVManager.exe", "payload/windows-browser.mjs"}
	if system == MacOS {
		essential = []string{"Cage Guest Window.app/Contents/MacOS/CageGuestWindow", "Cage Guest Clipboard.app/Contents/MacOS/CageGuestClipboard", "project/cage-sshfs", "project/cage-macos-terminal", "native-browser/macos-native-browser.mjs", "native-browser/macos-browser-service.mjs", "register-macos-browser.sh", "macos-cli.py", "cli_channel.py"}
	}
	for _, relative := range essential {
		info, err := os.Lstat(filepath.Join(kit, filepath.FromSlash(relative)))
		if err != nil || !info.Mode().IsRegular() {
			return manifest, fmt.Errorf("原生组件包缺少 %s", relative)
		}
	}
	err := filepath.WalkDir(kit, func(filename string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("原生组件目录不能包含文件链接")
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("原生组件目录包含特殊文件")
		}
		relative, err := filepath.Rel(kit, filename)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if relative == "native-components.json" {
			return nil
		}
		file, err := os.Open(filename)
		if err != nil {
			return err
		}
		hash := sha256.New()
		_, readErr := io.Copy(hash, file)
		if err = errors.Join(readErr, file.Close()); err != nil {
			return err
		}
		digest := hex.EncodeToString(hash.Sum(nil))
		target := nativeComponentTarget(relative, system)
		installed := digest
		if system == MacOS && relative == "claude-account.py" {
			data, err := os.ReadFile(filename)
			if err != nil {
				return err
			}
			_, body, found := bytes.Cut(data, []byte("\n"))
			if !found {
				return errors.New("账号组件缺少程序正文")
			}
			rewritten := sha256.Sum256(append([]byte("#!"+MacOSCLIRuntime+"/bin/python3 -I\n"), body...))
			installed = hex.EncodeToString(rewritten[:])
		}
		manifest.Files = append(manifest.Files, NativeComponentFile{Source: relative, Target: target, SHA256: digest, InstalledSHA256: installed})
		if system == Windows11 && relative == "payload/Cage.GuestClipboard.exe" {
			manifest.Files = append(manifest.Files, NativeComponentFile{Source: relative, Target: "network/Cage.GuestNetwork.exe", SHA256: digest, InstalledSHA256: installed})
		}
		return nil
	})
	if err != nil {
		return manifest, err
	}
	if len(manifest.Files) == 0 {
		return manifest, errors.New("此应用缺少原生客机组件")
	}
	sort.Slice(manifest.Files, func(i, j int) bool {
		if manifest.Files[i].Source != manifest.Files[j].Source {
			return manifest.Files[i].Source < manifest.Files[j].Source
		}
		return manifest.Files[i].Target < manifest.Files[j].Target
	})
	digest := sha256.New()
	fmt.Fprintf(digest, "cage-native-components-1\x00%s\x00", system)
	for _, file := range manifest.Files {
		fmt.Fprintf(digest, "%s\x00%s\x00%s\x00%s\x00", file.Source, file.Target, file.SHA256, file.InstalledSHA256)
	}
	manifest.Revision = hex.EncodeToString(digest.Sum(nil))
	manifest.Tools = map[string]string{"chromium": "系统维护的 Chrome / Edge", "code_server": "261c84cb1eddf11235f9e2c921425e9663fbe556"}
	if system == MacOS {
		manifest.Tools = map[string]string{"chromium": "Google Chrome 稳定版", "code_server": "4.145.0", "claude": "2.1.311", "node": "24.22.2", "python": "3.13.18", "git": "2.56.0"}
	} else {
		var dependencies map[string]struct {
			Version string `json:"version"`
		}
		if data, err := os.ReadFile(filepath.Join(kit, "payload", "cli-dependencies.json")); err != nil {
			return manifest, err
		} else if err = json.Unmarshal(data, &dependencies); err != nil {
			return manifest, err
		}
		for _, name := range []string{"claude", "python", "git"} {
			manifest.Tools[name] = dependencies[name].Version
		}
		manifest.Tools["node"] = "24.22.2"
	}
	return manifest, manifest.Validate()
}

func nativeComponentTarget(source, system string) string {
	if system == Windows11 {
		if !strings.HasPrefix(source, "payload/") {
			return ""
		}
		name := strings.TrimPrefix(source, "payload/")
		if name == "Cage.HyperVManager.exe" || name == "windows-workbench-packages.json" {
			return ""
		}
		return "bridge/" + name
	}
	switch {
	case strings.HasPrefix(source, "Cage ") && strings.Contains(source, ".app/"):
		return "/Applications/" + source
	case strings.HasPrefix(source, "project/CageProjectAccess.app/"):
		return "/Applications/" + strings.TrimPrefix(source, "project/")
	case strings.HasPrefix(source, "native-browser/"):
		return MacOSBrowserTools + "/controller/" + strings.TrimPrefix(source, "native-browser/")
	}
	targets := map[string]string{
		"project/cage-sshfs":              "/usr/local/libexec/cage-sshfs",
		"project/cage-macos-terminal":     "/usr/local/libexec/cage-macos-terminal",
		"cage-finish-initial-setup":       "/usr/local/libexec/cage-finish-initial-setup",
		"cage-macos-ipv6-check":           "/usr/local/libexec/cage-macos-ipv6-check",
		"macos-browser.mjs":               "/usr/local/libexec/cage-macos-browser.mjs",
		"cage-macos-editor":               "/usr/local/libexec/cage-macos-editor",
		"register-macos-claude-window.sh": "/usr/local/libexec/cage-register-claude-window",
		"register-macos-chrome-window.sh": "/usr/local/libexec/cage-register-chrome-window",
		"register-macos-clipboard.sh":     "/usr/local/libexec/cage-register-clipboard",
		"register-macos-browser.sh":       "/usr/local/libexec/cage-register-browser",
		"macos-cli.py":                    "/usr/local/libexec/cage-cli/macos-cli.py",
		"cli_channel.py":                  "/usr/local/libexec/cage-cli/cli_channel.py",
		"cli_channel.resume.md":           "/usr/local/libexec/cage-cli/cli_channel.resume.md",
		"claude_account_resume.md":        "/usr/local/libexec/cage-cli/cli_channel.account-resume.md",
		"claude-account.py":               "/usr/local/libexec/cage-cli/bin/cage-claude-account",
	}
	return targets[source]
}

func (m NativeComponentManifest) Validate() error {
	if m.Protocol != 1 || (m.System != MacOS && m.System != Windows11) || len(m.Revision) != 64 || len(m.Files) == 0 || len(m.Files) > 10000 {
		return errors.New("原生组件清单无效")
	}
	if _, err := hex.DecodeString(m.Revision); err != nil {
		return errors.New("原生组件版本摘要无效")
	}
	seen := map[string]bool{}
	for _, file := range m.Files {
		key := file.Source + "\x00" + file.Target
		if !fs.ValidPath(file.Source) || strings.ContainsAny(file.Source+file.Target, "\x00\r\n") || seen[key] || len(file.SHA256) != 64 || len(file.InstalledSHA256) != 64 {
			return errors.New("原生组件文件记录无效")
		}
		if _, err := hex.DecodeString(file.SHA256 + file.InstalledSHA256); err != nil {
			return errors.New("原生组件摘要无效")
		}
		if file.Target != "" && (path.Clean(file.Target) != file.Target || strings.Contains(file.Target, "\\")) {
			return errors.New("原生组件安装位置无效")
		}
		if file.Target != "" && seen["\x00"+file.Target] {
			return errors.New("原生组件安装位置重复")
		}
		if file.Target != "" && m.System == MacOS && !strings.HasPrefix(file.Target, "/Applications/Cage") && !strings.HasPrefix(file.Target, "/usr/local/libexec/cage-") {
			return errors.New("macOS 组件安装位置无效")
		}
		if file.Target != "" && m.System == Windows11 && !strings.HasPrefix(file.Target, "bridge/") && !strings.HasPrefix(file.Target, "network/") {
			return errors.New("Windows 组件安装位置无效")
		}
		if m.System == Windows11 && strings.ContainsAny(file.Source+file.Target, ":\\") {
			return errors.New("Windows 组件文件名无效")
		}
		seen[key] = true
		if file.Target != "" {
			seen["\x00"+file.Target] = true
		}
	}
	return nil
}
