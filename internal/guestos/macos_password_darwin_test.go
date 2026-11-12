package guestos

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Exercise the shipping native password policy without granting host permissions.
func TestMacPasswordAcceptsVerifiedSettingsExtensions(t *testing.T) {
	dir := t.TempDir()
	harness := `import AppKit
struct Failure: Error {
 let code: Int
 let message: String
 init(_ code: Int, _ message: String) { self.code = code; self.message = message }
}
func requireGuest() throws { throw Failure(1,"unused") }
func consoleUser() throws { throw Failure(1,"unused") }
@main struct Check {
 @MainActor static func main() throws { try MacSetupPassword.selfTest() }
}
`
	input := filepath.Join(dir, "check.swift")
	if err := os.WriteFile(input, []byte(harness), 0600); err != nil {
		t.Fatal(err)
	}
	program := filepath.Join(dir, "password-check")
	source := filepath.Join("..", "..", "scripts", "guest-os", "macos-setup-password.swift")
	maintenance := filepath.Join("..", "..", "scripts", "guest-os", "macos-maintenance.swift")
	if output, err := exec.CommandContext(t.Context(), "xcrun", "swiftc", "-parse-as-library", source, maintenance, input, "-o", program).CombinedOutput(); err != nil {
		t.Fatalf("compile password policy: %v\n%s", err, output)
	}
	if output, err := exec.CommandContext(t.Context(), program).CombinedOutput(); err != nil {
		t.Fatalf("password policy: %v\n%s", err, output)
	}
}
