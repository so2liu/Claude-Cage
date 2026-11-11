package workbench

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sylvinus/agent-vm/internal/guestos"
)

func TestMacOSClaudeRouteCannotRequestProjectPanel(t *testing.T) {
	s := testServer(t)
	body := `{"action":"project-frame","project":"/Users/guest/Cage/Projects/aaaaaaaaaaaaaaaaaaaaaaaa"}`
	// A project grant must be checked by the separate approval route.
	for _, test := range []struct {
		profile *guestos.MacOSProfile
		status  int
	}{
		{nil, http.StatusNotFound},
		{&guestos.MacOSProfile{Instance: "mac", User: "guest"}, http.StatusBadRequest},
	} {
		s.macosGuest = test.profile
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/api/macos/claude/window", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		s.macOSClaudeWindow(response, request)
		if response.Code != test.status {
			t.Fatalf("got %d, want %d", response.Code, test.status)
		}
	}
}

func TestNativeChromeTargetCannotBeSelectedThroughRequestJSON(t *testing.T) {
	var input macOSWindowRequest
	if err := json.Unmarshal([]byte(`{"action":"status","Chrome":true,"chrome":true}`), &input); err != nil {
		t.Fatal(err)
	}
	if input.Chrome {
		t.Fatal("client input selected native helper")
	}
	if !strings.Contains(nativeWindowSocketCommand(true, "30"), "dev.cage.GuestChromeWindow/window.sock") {
		t.Fatal("Chrome route did not select dedicated helper")
	}
	if strings.Contains(nativeWindowSocketCommand(false, "30"), "GuestChromeWindow") {
		t.Fatal("Claude route shared Chrome helper")
	}
	input.Chrome = true
	if !macOSWindowClientRequest(input, false).Chrome {
		t.Fatal("trusted route selection was lost")
	}
}

func TestNativeChromeURLOnlyAcceptsWebNavigation(t *testing.T) {
	for _, address := range []string{"https://claude.ai/login", "http://127.0.0.1:1234/test"} {
		if !validMacOSWindowRequest(macOSWindowRequest{Action: "open-url", URL: address}) {
			t.Fatalf("web URL rejected: %s", address)
		}
	}
	for _, address := range []string{"file:///etc/passwd", "javascript:alert(1)", "https://user:password@example.test/", "https:///missing-host", ""} {
		if validMacOSWindowRequest(macOSWindowRequest{Action: "open-url", URL: address}) {
			t.Fatal("unsafe navigation accepted")
		}
	}
}
