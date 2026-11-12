package workbench

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"reflect"
	"testing"
	"time"
)

func TestBrowserFocusRecoveryKeepsRFBDisplayAlive(t *testing.T) {
	frame := windowTestFrame(t, 2, 1)
	frame.ID, frame.Document = "tab", "doc"
	server, client := net.Pipe()
	defer client.Close()
	client.SetDeadline(time.Now().Add(5 * time.Second))

	go func() {
		defer server.Close()
		serveWindowsWindowRFB(server, frame, func(input map[string]any, output any) error {
			if input["action"] == "frame" {
				*output.(*windowsWindowFrame) = frame
				return nil
			}
			_, err := recoverBrowserInputFocus(context.Background(), func(_ context.Context, _, path string, _ any) ([]byte, error) {
				switch path {
				case "/input":
					return nil, windowsBrowserFailure{"browser_focus_required", "focus"}
				case "/tabs":
					return []byte(`{"active":"tab","tabs":[{"id":"tab","document":"doc"}]}`), nil
				default:
					return []byte(`{"ok":true}`), nil
				}
			}, input, func() {})
			return err
		})
	}()
	read := func(n int) []byte {
		t.Helper()
		b := make([]byte, n)
		if _, err := io.ReadFull(client, b); err != nil {
			t.Fatal(err)
		}
		return b
	}
	write := func(b []byte) {
		t.Helper()
		if _, err := client.Write(b); err != nil {
			t.Fatal(err)
		}
	}
	read(12)
	write([]byte("RFB 003.008\n"))
	read(2)
	write([]byte{1})
	read(4)
	write([]byte{1})
	init := read(24)
	read(int(binary.BigEndian.Uint32(init[20:])))
	// Input is accepted only after the first frame, so establish it first.
	write([]byte{3, 0, 0, 0, 0, 0, 0, 2, 0, 1})
	read(24)
	// A rejected mouse event followed by a rejected key event must not close
	// the display socket; the next frame must still be delivered.
	write([]byte{5, 1, 0, 1, 0, 0})
	write([]byte{4, 1, 0, 0, 0, 0, 0, 97})
	write([]byte{3, 0, 0, 0, 0, 0, 0, 2, 0, 1})
	update := read(16)
	if binary.BigEndian.Uint16(update[2:]) != 1 {
		t.Fatal("display frame missing after focus rejection")
	}
	read(8)
}

func TestBrowserFrameTransitionRetry(t *testing.T) {
	for _, tc := range []struct {
		name, code      string
		failures, calls int
		fatal           bool
	}{
		{"transient", "browser_target_changed", 1, 2, false},
		{"document transition", "target_changed", 1, 2, false},
		{"popup capture transition", "desktop_target_changed", 2, 3, false},
		{"popup capture busy", "desktop_capture_busy", 1, 2, false},
		{"persistent", "browser_target_changed", 8, 6, true},
		{"permission", "desktop_capture_permission_required", 1, 1, true},
		{"locked", "requires_gui_login", 1, 1, true},
		{"identity", "browser_process_unavailable", 1, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			data, err := requestBrowserFrame(context.Background(), func(_ context.Context, method, path string, _ any) ([]byte, error) {
				if method != "GET" || path != "/frame" {
					t.Fatal("retried a mutation")
				}
				calls++
				if calls <= tc.failures {
					return nil, windowsBrowserFailure{tc.code, "rejected"}
				}
				return []byte("fresh validated frame"), nil
			}, map[string]any{"action": "frame"})
			if calls != tc.calls || (err != nil) != tc.fatal {
				t.Fatalf("calls=%d error=%v", calls, err)
			}
			if !tc.fatal && string(data) != "fresh validated frame" {
				t.Fatal("missing fresh frame")
			}
		})
	}
}

func TestBrowserFrameTransitionCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	_, err := requestBrowserFrame(ctx, func(context.Context, string, string, any) ([]byte, error) {
		calls++
		cancel()
		return nil, windowsBrowserFailure{"browser_target_changed", "changed"}
	}, nil)
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestBrowserFocusRecoveryDoesNotReplayInput(t *testing.T) {

	var calls []string
	attempts := 0
	released := 0
	request := func(_ context.Context, method, path string, input any) ([]byte, error) {
		calls = append(calls, method+" "+path)
		switch path {
		case "/input":
			attempts++
			if attempts == 1 {
				return nil, windowsBrowserFailure{"browser_focus_required", "focus rejected"}
			}
			return []byte(`{"ok":true}`), nil
		case "/tabs":
			return []byte(`{"active":"tab","tabs":[{"id":"tab","document":"doc"}]}`), nil
		case "/activate":
			if input.(map[string]any)["id"] != "tab" {
				t.Fatal("activated wrong tab")
			}
			return []byte(`{"ok":true}`), nil
		}
		t.Fatalf("unexpected request %s", path)
		return nil, nil
	}
	input := map[string]any{"kind": "key", "target": "geometry", "down": true}
	if _, err := recoverBrowserInputFocus(context.Background(), request, input, func() { released++ }); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || released != 1 {
		t.Fatal("input was replayed or its lease was not released")
	}
	if !reflect.DeepEqual(calls, []string{"POST /input"}) {
		t.Fatal(calls)
	}
	if _, err := recoverBrowserInputFocus(context.Background(), request, input, func() { released++ }); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 || released != 1 {
		t.Fatal("next user input was not accepted")
	}
}

func TestBrowserFocusRecoveryPreservesBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, code, tabs string
		fatal            bool
	}{
		{"other tab", "browser_focus_required", `{"active":"other","tabs":[{"id":"tab","document":"doc"}]}`, false},
		{"new document", "browser_focus_required", `{"active":"tab","tabs":[{"id":"tab","document":"new"}]}`, false},
		{"target changed", "browser_target_changed", "", false},
		{"document changed", "target_changed", "", false},
		{"locked session", "requires_gui_login", "", true},
		{"transport failure", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {

			failure := error(windowsBrowserFailure{tc.code, "rejected"})
			if tc.code == "" {
				failure = errors.New("EOF")
			}
			request := func(_ context.Context, method, path string, _ any) ([]byte, error) {
				switch path {
				case "/input":
					return nil, failure
				case "/tabs":
					return []byte(tc.tabs), nil
				default:
					t.Fatalf("unexpected recovery %s %s", method, path)
					return nil, nil
				}
			}
			_, err := recoverBrowserInputFocus(context.Background(), request, map[string]any{}, func() {})
			if (err != nil) != tc.fatal {
				t.Fatalf("fatal=%v err=%v", tc.fatal, err)
			}
		})
	}
}

func TestBrowserFocusRecoveryNeverStealsFocus(t *testing.T) {

	request := func(_ context.Context, _, path string, _ any) ([]byte, error) {
		if path != "/input" {
			t.Fatal("focus recovery flooded")
		}
		return nil, windowsBrowserFailure{"browser_focus_required", "focus"}
	}
	_, err := recoverBrowserInputFocus(context.Background(), request, map[string]any{}, func() {})
	if err != nil {
		t.Fatal(err)
	}
}

// An older macOS guest runs its browser on the connection every pane shares.
// With nothing safe to release, the rejection stays an error and the display
// reconnects as it did before.
func TestBrowserFocusRejectionStaysAnErrorWithoutAReleasableConnection(t *testing.T) {
	failure := windowsBrowserFailure{"target_changed", "changed"}
	_, err := recoverBrowserInputFocus(context.Background(), func(context.Context, string, string, any) ([]byte, error) {
		return nil, failure
	}, map[string]any{}, nil)
	if !errors.Is(err, failure) {
		t.Fatalf("rejection hidden: %v", err)
	}
}
