package main

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/cli"
	"github.com/sylvinus/agent-vm/internal/version"
)

func TestEntryDropsInheritedUnsafeSwitchesBeforeDispatch(t *testing.T) {
	// A benign early-return command exercises the entry boundary without a VM,
	// Git configuration changes, or access to the user's Cage profile.
	unsafe := []string{
		"AGENT_VM_UNSAFE_OPEN_NETWORK",
		"AGENT_VM_UNSAFE_WRITABLE_GIT",
		"AGENT_VM_UNSAFE_DISABLE_SECURITY_PROMPTS",
	}
	for _, name := range unsafe {
		t.Setenv(name, "1")
	}
	t.Setenv("CLAUDE_CAGE_CONFIG", "/not-accessed-by-help/config.json")
	previous := os.Args
	os.Args = []string{"cage", "help"}
	t.Cleanup(func() { os.Args = previous })
	if code := run(); code != 0 {
		t.Fatalf("help returned %d", code)
	}
	for _, name := range unsafe {
		if _, exists := os.LookupEnv(name); exists {
			t.Errorf("%s survived the Cage entry point", name)
		}
	}
	if got := os.Getenv("CLAUDE_CAGE_CONFIG"); got != "/not-accessed-by-help/config.json" {
		t.Errorf("profile selection changed: %q", got)
	}
}

func TestGuestServiceStartStreamsScriptAndPreservesInteractiveInput(t *testing.T) {
	script := cage.DefaultConfig().StartScript()
	t.Logf("service payload: %d bytes, %d UTF-16 code units", len(script), len(utf16.Encode([]rune(script))))
	if len(utf16.Encode([]rune(script))) <= 32767 {
		t.Fatal("fixture no longer exercises the Windows command-line limit")
	}
	input := strings.NewReader("interactive input must remain available")
	var output, stderr bytes.Buffer
	stdio := cli.IO{Stdin: input, Stdout: &output, Stderr: &stderr}
	code := runGuestServiceStart(script, stdio, func(args []string, streams cli.IO) int {
		if !slices.Equal(args, []string{"run", "bash", "-s"}) {
			t.Fatalf("unexpected service command: %q", args)
		}
		// Use a real child process to verify transport on Windows without
		// executing guest provisioning against the host filesystem.
		cmd := exec.Command(os.Args[0], append([]string{"-test.run=^TestGuestServiceScriptCaptureProcess$", "--"}, args...)...)
		cmd.Env = append(os.Environ(), "CAGE_TEST_SCRIPT_CAPTURE=1")
		cmd.Stdin, cmd.Stdout, cmd.Stderr = streams.Stdin, streams.Stdout, streams.Stderr
		err := cmd.Run()
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode()
		}
		if err != nil {
			t.Fatal(err)
		}
		return 0
	})
	if code != 17 || output.String() != fmt.Sprintf("%x", sha256.Sum256([]byte(script))) {
		t.Fatalf("script transport or exit status changed: code=%d stdout=%q stderr=%q", code, output.String(), stderr.String())
	}
	remaining, err := io.ReadAll(stdio.Stdin)
	if err != nil || string(remaining) != "interactive input must remain available" {
		t.Fatalf("internal service startup consumed interactive stdin: %q, %v", remaining, err)
	}
}

func TestGuestServiceScriptCaptureProcess(t *testing.T) {
	if os.Getenv("CAGE_TEST_SCRIPT_CAPTURE") != "1" {
		t.Skip("helper process for script transport")
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(18)
	}
	fmt.Printf("%x", sha256.Sum256(data))
	os.Exit(17)
}

// TestStatelessCommandsNeedNoProfile covers what an installer or a packaging
// script runs before a profile exists: these commands must answer without
// reading, creating or logging into the Cage profile.
func TestStatelessCommandsNeedNoProfile(t *testing.T) {
	if version.Version == "" {
		t.Fatal("this test build has no version")
	}
	cases := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{name: "help", args: []string{"help"}, wantStdout: "Claude Cage"},
		{name: "short help", args: []string{"-h"}, wantStdout: "Claude Cage"},
		{name: "version", args: []string{"version"}, wantStdout: version.Version},
		{name: "long version", args: []string{"--version"}, wantStdout: version.Version},
		{name: "short version", args: []string{"-V"}, wantStdout: version.Version},
		{name: "version gate met", args: []string{"version", "--min", "0.0.0"}},
		{name: "version gate missed", args: []string{"version", "--min", "99.0.0"}, wantCode: 1, wantStderr: "older than"},
		{name: "licenses", args: []string{"licenses"}, wantStdout: "MIT License"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			profileDir := filepath.Join(t.TempDir(), "profile")
			home := t.TempDir()
			t.Setenv("CLAUDE_CAGE_CONFIG", filepath.Join(profileDir, "config.json"))
			t.Setenv("HOME", home)

			previous := os.Args
			os.Args = append([]string{"cage"}, tc.args...)
			t.Cleanup(func() { os.Args = previous })

			code, stdout, stderr := captureRun(t, run)
			if code != tc.wantCode {
				t.Fatalf("exit %d, want %d (stdout %q, stderr %q)", code, tc.wantCode, stdout, stderr)
			}
			if tc.wantStdout != "" && !strings.Contains(stdout, tc.wantStdout) {
				t.Errorf("stdout %q does not contain %q", stdout, tc.wantStdout)
			}
			if tc.wantStderr != "" && !strings.Contains(stderr, tc.wantStderr) {
				t.Errorf("stderr %q does not contain %q", stderr, tc.wantStderr)
			}
			if _, err := os.Stat(profileDir); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("profile directory was created: %v", err)
			}
			if _, err := os.Stat(filepath.Join(home, ".claude-cage")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("home profile was created")
			}
		})
	}
}

// captureRun redirects the process streams for the entry point, which writes
// through os.Stdout, os.Stderr and cli.Stdio.
func captureRun(t *testing.T, fn func() int) (int, string, string) {
	t.Helper()
	directory := t.TempDir()
	stdout, err := os.Create(filepath.Join(directory, "stdout"))
	if err != nil {
		t.Fatal(err)
	}
	stderr, err := os.Create(filepath.Join(directory, "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	previousStdout, previousStderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = stdout, stderr
	defer func() { os.Stdout, os.Stderr = previousStdout, previousStderr }()

	code := fn()
	for _, file := range []*os.File{stdout, stderr} {
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}
	out, err := os.ReadFile(stdout.Name())
	if err != nil {
		t.Fatal(err)
	}
	errOut, err := os.ReadFile(stderr.Name())
	if err != nil {
		t.Fatal(err)
	}
	return code, string(out), string(errOut)
}
func TestContainerInstallsGitHubCLIByDefault(t *testing.T) {
	script, err := containerInstallScript("tools")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(script, "export AGENT_VM_INSTALL_GH=1\n") {
		t.Fatal("fresh cloud environments would omit GitHub CLI")
	}
}
