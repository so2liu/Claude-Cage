package workbench

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestServeShutdownHandoff(t *testing.T) {
	const helperConfig = "CAGE_TEST_SHUTDOWN_CONFIG"
	if configPath := os.Getenv(helperConfig); configPath != "" {
		if err := Serve(configPath); err != nil {
			t.Fatal(err)
		}
		return
	}
	for _, tc := range []struct {
		name, saved                  string
		timezone, language, balanced bool
	}{
		{"fresh profile", "", true, true, true},
		{"legacy missing fields", `{"keyboard":"native"}`, true, false, false},
		{"saved manual choice", `{"auto_timezone":false,"auto_language":false}`, false, false, false},
		{"saved automatic choice", `{"auto_timezone":true,"auto_language":true}`, true, true, false},
		{"saved sharp display", `{"balanced_display":false}`, true, false, false},
		{"saved balanced display", `{"balanced_display":true}`, true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) { testServeShutdownHandoff(t, tc.saved, tc.timezone, tc.language, tc.balanced) })
	}
}

func testServeShutdownHandoff(t *testing.T, saved string, timezone, language, balanced bool) {
	const helperConfig = "CAGE_TEST_SHUTDOWN_CONFIG"

	// Keep both the profile and its sibling workspace under the test directory.
	// With no VM record, node store or service record, Serve only initializes
	// private files; its monitors and handoff cannot reach a running environment.
	profile := filepath.Join(t.TempDir(), "profile")
	configPath := filepath.Join(profile, "config.json")
	if saved != "" {
		if err := os.MkdirAll(profile, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(profile, "workbench.json"), []byte(saved), 0600); err != nil {
			t.Fatal(err)
		}
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestServeShutdownHandoff$", "-test.timeout=25s")
	cmd.Env = append(os.Environ(), helperConfig+"="+configPath)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var waitErr error
	go func() {
		waitErr = cmd.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		if t.Failed() {
			t.Logf("control service output: %s", output.String())
		}
	})

	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: time.Second}
	var record controlFile
	var origin string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-done:
			t.Fatalf("control service exited before readiness: %v", waitErr)
		default:
		}
		data, err := os.ReadFile(filepath.Join(profile, "control.json"))
		if err == nil && json.Unmarshal(data, &record) == nil && record.PID == cmd.Process.Pid && record.Port > 0 && record.Token != "" {
			origin = fmt.Sprintf("http://localhost:%d", record.Port)
			response, err := client.Get(origin + "/healthz")
			if err == nil {
				_, _ = io.Copy(io.Discard, response.Body)
				response.Body.Close()
				if response.StatusCode == http.StatusOK {
					break
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if origin == "" || time.Now().After(deadline) {
		t.Fatal("control service did not become ready")
	}
	preferencesData, err := os.ReadFile(filepath.Join(profile, "workbench.json"))
	var preferences Preferences
	if err != nil || json.Unmarshal(preferencesData, &preferences) != nil {
		t.Fatalf("initialized preferences: %s, %v", preferencesData, err)
	}
	if preferences.AutoTimezone != timezone || preferences.AutoLanguage != language {
		t.Fatalf("automatic defaults/override: timezone=%t language=%t", preferences.AutoTimezone, preferences.AutoLanguage)
	}
	if preferences.BalancedDisplay != balanced {
		t.Fatalf("display default/override: balanced=%t", preferences.BalancedDisplay)
	}

	lock, err := os.OpenFile(filepath.Join(profile, "control.lock"), os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := lockController(lock); err == nil {
		_ = unlockController(lock)
		t.Fatal("live control service did not hold its profile lock")
	}
	data, err := json.Marshal(map[string]string{"token": record.Token})
	if err != nil {
		t.Fatal(err)
	}
	// A monitor may briefly own the operation lock during readiness. Retry only
	// the documented busy response; authentication and other failures are final.
	deadline = time.Now().Add(3 * time.Second)
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, origin+"/api/handoff", bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Origin", origin)
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(io.LimitReader(response.Body, 4096))
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode == http.StatusOK {
			var result struct {
				Stopping bool `json:"stopping"`
			}
			if json.Unmarshal(body, &result) != nil || !result.Stopping {
				t.Fatalf("handoff did not acknowledge shutdown: %s", body)
			}
			break
		}
		if response.StatusCode != http.StatusConflict || time.Now().After(deadline) {
			t.Fatalf("handoff failed: HTTP %d %s", response.StatusCode, body)
		}
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case <-done:
		if waitErr != nil {
			t.Fatalf("control service exit: %v", waitErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handoff did not drain the control service")
	}
	if err := lockController(lock); err != nil {
		t.Fatalf("control service did not release its profile lock: %v", err)
	}
	defer unlockController(lock)
	if _, err := os.Stat(filepath.Join(profile, "state", "lima")); !os.IsNotExist(err) {
		t.Fatalf("idle control service unexpectedly created VM state: %v", err)
	}
}
