package workbench

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestClaudeDesktopSnapshotFailureReason(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("snapshot executes in the Linux guest; fixture requires POSIX executables")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable")
	}
	dir := t.TempDir()
	for name, script := range map[string]string{
		"dpkg-query": "#!/bin/sh\nprintf 'install ok installed\\n4.4212.0\\n'\n",
		"systemctl":  "#!/bin/sh\nif [ \"$1\" = is-active ]; then printf '%s\\n' \"$TEST_ACTIVE\"; else printf '%s\\n' \"$TEST_RESULT\"; fi\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0755); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ active, result, want string }{
		{"failed", "oom-kill", "oom-kill"}, {"failed", "exit-code", ""}, {"active", "oom-kill", ""},
	} {
		cmd := exec.Command("sh", "-c", claudeDesktopSnapshot)
		cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "TEST_ACTIVE="+tc.active, "TEST_RESULT="+tc.result)
		out, err := cmd.CombinedOutput()
		var state claudeDesktopState
		if err != nil || json.Unmarshal(out, &state) != nil {
			t.Fatalf("snapshot: %v %s", err, out)
		}
		if state.FailureReason != tc.want || state.Failed != (tc.active == "failed") || state.Running != (tc.active == "active") {
			t.Fatalf("%+v: %+v", tc, state)
		}
	}
}

func TestClaudeDesktopControlBoundary(t *testing.T) {
	s := testServer(t)
	for _, path := range []string{"/api/claude-desktop/install", "/api/claude-desktop/start", "/api/claude-desktop/stop"} {
		if w := request(s, "POST", path, s.origin, "{}", false); w.Code != 401 {
			t.Fatalf("%s without session: %d", path, w.Code)
		}
		if w := request(s, "POST", path, "https://claude.ai", "{}", true); w.Code != 403 {
			t.Fatalf("%s cross-origin: %d", path, w.Code)
		}
		if w := request(s, "POST", path, s.origin, `{"command":"touch /tmp/untrusted"}`, true); w.Code != 400 {
			t.Fatalf("%s accepted arbitrary command: %d", path, w.Code)
		}
		if w := request(s, "POST", path, s.origin, "{}", true); w.Code != 409 {
			t.Fatalf("%s must require running VM: %d", path, w.Code)
		}
	}
	for _, body := range []string{`{"width":1,"height":1000}`, `{"width":8193,"height":1000}`, `{"width":1000,"height":8193}`, `{"width":"1024;command","height":768}`,
		`{"width":1000,"height":800,"scale":0.5}`, `{"width":1000,"height":800,"scale":4.5}`, `{"width":1000,"height":800,"scale":"2;command"}`} {
		if w := request(s, "POST", "/api/claude-desktop/resize", s.origin, body, true); w.Code != 400 {
			t.Fatalf("invalid geometry: %d", w.Code)
		}
	}
}
