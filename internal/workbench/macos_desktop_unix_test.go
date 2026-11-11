//go:build !windows

package workbench

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/guestos"
)

func TestMacDesktopCapabilityBoundaryAndRevocation(t *testing.T) {
	s := testServer(t)
	profile, err := os.MkdirTemp("/tmp", "cage-desktop-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(profile)
	s.profile = profile
	s.macosGuest = &guestos.MacOSProfile{Instance: "mac", User: "trial"}
	s.ready = true
	if err := os.MkdirAll(s.instanceDir(), 0700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(s.instanceDir(), "cage-login.sock"))
	if err != nil {
		t.Fatal(err)
	}
	requests := make(chan guestos.MacLoginRequest, 8)
	var legacy atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			var input guestos.MacLoginRequest
			if json.NewDecoder(conn).Decode(&input) == nil {
				requests <- input
				result := guestos.MacLoginReply{DesktopVersion: 1}
				if input.Action == "desktop-open" {
					result.Session = "desktop-fixture"
				}
				if legacy.Load() {
					result = guestos.MacLoginReply{Closed: true}
				}
				_ = json.NewEncoder(conn).Encode(result)
			}
			conn.Close()
		}
	}()
	defer func() { listener.Close(); <-done }()
	post := func(path, body string, trusted bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, s.origin+path, strings.NewReader(body))
		req.Header.Set("Origin", s.origin)
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: s.cookieName(), Value: s.session})
		if trusted {
			req.AddCookie(&http.Cookie{Name: s.cookieName() + "-desktop", Value: trustedViewProof(s.token)})
		}
		response := httptest.NewRecorder()
		s.handler().ServeHTTP(response, req)
		return response
	}
	for _, path := range []string{"/api/macos/login", "/api/macos/maintenance", "/api/macos/first-setup"} {
		result := post(path, `{"action":"desktop-open","maintenance":"projects"}`, true)
		if result.Code < 400 {
			t.Fatalf("limited endpoint opened desktop: %s", path)
		}
	}
	if result := post("/api/macos/desktop", `{"action":"desktop-open"}`, false); result.Code != http.StatusForbidden {
		t.Fatalf("untrusted desktop accepted: %d", result.Code)
	}
	if result := post("/api/macos/desktop", `{"action":"frame"}`, true); result.Code != http.StatusBadRequest {
		t.Fatalf("desktop accepted capture controls: %d", result.Code)
	}
	legacy.Store(true)
	result := post("/api/macos/desktop", `{"action":"desktop-open"}`, true)
	<-requests
	var reply guestos.MacLoginReply
	if json.Unmarshal(result.Body.Bytes(), &reply) != nil || reply.Recovery != "environment-restart" || !reply.Closed {
		t.Fatal("legacy runner did not require restart")
	}
	legacy.Store(false)
	result = post("/api/macos/desktop", `{"action":"desktop-open","password":"injected","initial_setup":true,"maintenance":"projects"}`, true)
	if result.Code != http.StatusOK {
		t.Fatalf("desktop open failed: %d %s", result.Code, result.Body.String())
	}
	input := <-requests
	if input.Action != "desktop-open" || input.Password != "" || input.Setup || input.Maintenance != "" {
		t.Fatal("desktop capability retained login controls")
	}
	s.account.lock()
	select {
	case input = <-requests:
		if input.Action != "desktop-close" || input.Session != "desktop-fixture" {
			t.Fatal("account lock did not close the native desktop")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("account lock retained the native desktop")
	}
}
