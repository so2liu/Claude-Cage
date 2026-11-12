package guestos

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Compile the real shared settings router without running maintenance or
// touching user permissions. Both first setup and later maintenance use it.
func TestMacSettingsRoutesOpenSpecificPermissionPanes(t *testing.T) {
	dir := t.TempDir()
	harness := `import AppKit
struct Failure: Error { init(_ code: Int, _ reason: String) {} }
func consoleUser() throws -> String { throw Failure(1,"unused") }
func requireGuest() throws { throw Failure(1,"unused") }
enum MacSetupPassword {
    static func runStream() -> Never { fatalError("unused") }
    static func passwordFocused(_ app: NSRunningApplication, window: CGRect) -> Bool { false }
}
@main struct Check {
    @MainActor static func main() {
        for purpose in ["projects", "screen", "input"] {
            guard let url = MacMaintenance.settingsURL(for: purpose) else { exit(1) }
            print("\(purpose) \(url.absoluteString)")
        }
        guard MacMaintenance.settingsURL(for: "untrusted") == nil else { exit(2) }
    }
}
`
	input := filepath.Join(dir, "check.swift")
	if err := os.WriteFile(input, []byte(harness), 0600); err != nil {
		t.Fatal(err)
	}
	program := filepath.Join(dir, "settings-check")
	source := filepath.Join("..", "..", "scripts", "guest-os", "macos-maintenance.swift")
	compile := exec.CommandContext(t.Context(), "xcrun", "swiftc", "-parse-as-library", source, input, "-o", program)
	if output, err := compile.CombinedOutput(); err != nil {
		t.Fatalf("compile settings router: %v\n%s", err, output)
	}
	output, err := exec.CommandContext(t.Context(), program).CombinedOutput()
	if err != nil {
		t.Fatalf("settings routes: %v\n%s", err, output)
	}
	want := []string{
		"projects x-apple.systempreferences:com.apple.ExtensionsPreferences?extensionPointIdentifier=com.apple.fskit.fsmodule",
		"screen x-apple.systempreferences:com.apple.preference.security?Privacy_ScreenCapture",
		"input x-apple.systempreferences:com.apple.preference.security?Privacy_Accessibility",
	}
	if strings.TrimSpace(string(output)) != strings.Join(want, "\n") {
		t.Fatalf("permission routes lost the specific destination: %s", output)
	}
}
