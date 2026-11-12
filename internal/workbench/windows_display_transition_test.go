package workbench

import (
	"context"
	"errors"
	"testing"
)

func TestDesktopFrameTransitionRetry(t *testing.T) {
	for _, tc := range []struct {
		code            string
		failures, calls int
		fatal           bool
	}{
		{"desktop_target_changed", 2, 3, false},
		{"desktop_empty_frame", 1, 2, false},
		{"desktop_capture_busy", 1, 2, false},
		{"desktop_target_changed", 8, 6, true},
		{"desktop_capture_permission_required", 1, 1, true},
		{"desktop_registration_invalid", 1, 1, true},
		{"requires_gui_login", 1, 1, true},
	} {
		t.Run(tc.code, func(t *testing.T) {
			calls := 0
			var frame windowsWindowFrame
			err := requestDesktopFrame(context.Background(), func(input, output any) error {
				if input.(map[string]any)["action"] != "frame" {
					t.Fatal("retried mutation")
				}
				calls++
				if calls <= tc.failures {
					return windowsWindowFailure{tc.code}
				}
				*output.(*windowsWindowFrame) = windowsWindowFrame{Target: "fresh", Width: 2, Height: 1}
				return nil
			}, map[string]any{"action": "frame"}, &frame)
			if calls != tc.calls || (err != nil) != tc.fatal {
				t.Fatalf("calls=%d error=%v", calls, err)
			}
			if !tc.fatal && frame.Target != "fresh" {
				t.Fatal("missing fresh validated frame")
			}
		})
	}
}

func TestDesktopFrameTransitionCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := requestDesktopFrame(ctx, func(any, any) error { calls++; cancel(); return windowsWindowFailure{"desktop_capture_busy"} }, nil, nil)
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("calls=%d error=%v", calls, err)
	}
}

func TestDesktopFrameTransportFailureNotRetried(t *testing.T) {
	failure := errors.New("disconnected")
	calls := 0
	err := requestDesktopFrame(context.Background(), func(any, any) error { calls++; return failure }, nil, nil)
	if err != failure || calls != 1 {
		t.Fatal("transport failure hidden")
	}
}

func TestDesktopViewerDropsOldInputUntilFullFrame(t *testing.T) {
	calls, released := 0, 0
	unchanged := true
	request := desktopViewerRequest(context.Background(), func(input, output any) error {
		calls++
		if input.(map[string]any)["action"] == "frame" {
			if _, exists := input.(map[string]any)["after"]; exists {
				t.Fatal("must request a full fresh frame")
			}
			*output.(*windowsWindowFrame) = windowsWindowFrame{Unchanged: unchanged, Target: "new"}
			return nil
		}
		return windowsWindowFailure{"desktop_target_changed"}
	}, func() error { released++; return nil })
	if err := request(map[string]any{"action": "pointer"}, nil); err != nil {
		t.Fatal(err)
	}
	// The guest keeps a rejected viewer's keys held until its connection ends.
	if released != 1 {
		t.Fatal("held input was not released")
	}
	for _, action := range []string{"pointer", "key", "text"} {
		if err := request(map[string]any{"action": action}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 || released != 1 {
		t.Fatal("stale input was replayed")
	}
	var frame windowsWindowFrame
	if err := request(map[string]any{"action": "frame", "after": "old"}, &frame); err != nil {
		t.Fatal(err)
	}
	request(map[string]any{"action": "text"}, nil)
	if calls != 2 {
		t.Fatal("unchanged frame resumed input")
	}
	unchanged = false
	if err := request(map[string]any{"action": "frame", "after": "old"}, &frame); err != nil {
		t.Fatal(err)
	}
	request(map[string]any{"action": "key"}, nil)
	if calls != 4 {
		t.Fatal("fresh frame did not resume new input")
	}
}

func TestDesktopViewerPreservesFatalErrors(t *testing.T) {
	for _, code := range []string{"requires_gui_login", "desktop_registration_invalid", "desktop_pointer_invalid"} {
		request := desktopViewerRequest(context.Background(), func(any, any) error { return windowsWindowFailure{code} },
			func() error { t.Fatal("released input for", code); return nil })
		if request(map[string]any{"action": "pointer"}, nil) == nil {
			t.Fatal("hidden security failure", code)
		}
	}
}

func TestDesktopViewerFailsWhenHeldInputCannotBeReleased(t *testing.T) {
	failure := errors.New("relay unavailable")
	request := desktopViewerRequest(context.Background(), func(any, any) error { return windowsWindowFailure{"desktop_target_changed"} },
		func() error { return failure })
	// The display then reconnects, which releases the keys as before.
	if err := request(map[string]any{"action": "key"}, nil); err != failure {
		t.Fatalf("stuck input hidden: %v", err)
	}
}
