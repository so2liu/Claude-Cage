package workbench

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTerminalLoginLinkUsesGuestPrivateBrowser(t *testing.T) {
	const address = "https://claude.ai/oauth/authorize?state=fixture"
	s := testServer(t)
	s.ready = true
	terminal := newTerminalSession(nil)
	terminal.id = "1234567890abcdef"
	s.terminals.sessions = map[string]*terminalSession{terminal.id: terminal}
	finished := make(chan struct{})
	guest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct{ URL, Login string }
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.Login != terminal.id {
			t.Error("incorrect login request")
		}
		switch r.URL.Path {
		case "/login-open":
			if in.URL != address {
				t.Error("lost authorization parameters")
			}
			_, _ = w.Write([]byte(`{"id":"tab","url":"private"}`))
		case "/login-finish":
			if in.URL != "" {
				t.Error("cleanup exposed URL")
			}
			_, _ = w.Write([]byte(`{"ok":true}`))
			close(finished)
		default:
			t.Error("unexpected browser route")
		}
	}))
	defer guest.Close()
	s.browserControl = localBrowserClient(t, s.lifetimeContext(), guest)
	defer s.browserControl.close()
	for _, invalid := range []string{"http://claude.ai/oauth/authorize?x=1", "https://claude.ai:90/oauth/authorize?x=1", "https://claude.ai@elsewhere.test/oauth/authorize?x=1", "https://claude.ai/redirect?x=1", "https://claude.ai/oauth/authorize"} {
		if claudeAuthorizationURL(invalid) {
			t.Fatalf("accepted %q", invalid)
		}
	}
	w := request(s, "POST", "/api/terminals/"+terminal.id+"/login-link", s.origin, `{"url":"`+address+`"}`, true)
	if w.Code != 200 || w.Body.String() != "{\"id\":\"tab\"}\n" {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	close(terminal.done)
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("closing the terminal retained its login tab")
	}
}
