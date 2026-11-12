package guestos

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func nativeManifestKit(t *testing.T, system string) string {
	t.Helper()
	kit := t.TempDir()
	names := []string{"payload/Cage.GuestClipboard.exe", "payload/cage-guest-files.exe", "payload/cage-guest-boot.exe", "payload/Cage.HyperVManager.exe", "payload/windows-browser.mjs", "payload/cli-dependencies.json"}
	if system == MacOS {
		names = []string{"Cage Guest Window.app/Contents/MacOS/CageGuestWindow", "Cage Guest Clipboard.app/Contents/MacOS/CageGuestClipboard", "project/cage-sshfs", "project/cage-macos-terminal", "native-browser/macos-native-browser.mjs", "native-browser/macos-browser-service.mjs", "register-macos-browser.sh", "macos-cli.py", "cli_channel.py", "claude-account.py"}
	}
	for _, name := range names {
		filename := filepath.Join(kit, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(filename), 0700); err != nil {
			t.Fatal(err)
		}
		data := []byte("#!fixture\n{}")
		if name == "payload/cli-dependencies.json" {
			data = []byte(`{"claude":{"version":"2.1.311"},"python":{"version":"3.13.18"},"git":{"version":"2.56.0"},"gh":{"version":"2.103.3"}}`)
		}
		if err := os.WriteFile(filename, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return kit
}

func TestNativeManifestTracksInstalledComponents(t *testing.T) {
	for _, system := range []string{MacOS, Windows11} {
		t.Run(system, func(t *testing.T) {
			kit := nativeManifestKit(t, system)
			first, err := RequiredNativeComponents(kit, system)
			if err != nil {
				t.Fatal(err)
			}
			again, err := RequiredNativeComponents(kit, system)
			if err != nil || first.Revision != again.Revision {
				t.Fatalf("manifest is unstable: %v", err)
			}
			source := "payload/new-phone-browser.mjs"
			if system == MacOS {
				source = "native-browser/macos-phone-browser.mjs"
			}
			if err = os.WriteFile(filepath.Join(kit, filepath.FromSlash(source)), []byte("phone-browser"), 0600); err != nil {
				t.Fatal(err)
			}
			next, err := RequiredNativeComponents(kit, system)
			if err != nil || next.Revision == first.Revision {
				t.Fatalf("new component was omitted: %v", err)
			}
			found, packet := false, false
			for _, item := range next.Files {
				if item.Source == source {
					found = item.Target != "" && item.SHA256 == item.InstalledSHA256
				}
				if item.Target == "network/Cage.GuestNetwork.exe" {
					packet = item.Source == "payload/Cage.GuestClipboard.exe"
				}
				if item.Source == "claude-account.py" {
					expected := sha256.Sum256([]byte("#!" + MacOSCLIRuntime + "/bin/python3 -I\n{}"))
					if item.InstalledSHA256 != hex.EncodeToString(expected[:]) {
						t.Fatal("installed Python entry point digest differs from installer")
					}
				}
			}
			if !found || (system == Windows11 && !packet) {
				t.Fatal("manifest did not include installed guest capabilities")
			}
			if err = os.Symlink(source, filepath.Join(kit, "unexpected-link")); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			if _, err = RequiredNativeComponents(kit, system); err == nil {
				t.Fatal("linked payload was accepted")
			}
		})
	}
}

func TestNativeManifestRejectsEscapedTargets(t *testing.T) {
	manifest, err := RequiredNativeComponents(nativeManifestKit(t, MacOS), MacOS)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"/usr/local/libexec/cage-cli/../../private", "/etc/passwd", "network/Cage.GuestNetwork.exe"} {
		manifest.Files[0].Target = target
		if manifest.Validate() == nil {
			t.Fatalf("accepted target %q", target)
		}
	}
}
