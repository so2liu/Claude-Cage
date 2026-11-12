package workbench

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestTerminalStartupStatePreservesStreamAndReconnect(t *testing.T) {
	s := testServer(t)
	t.Cleanup(s.terminals.close)
	session, err := s.terminals.start(terminalChildCommand("echo"), "/project", "claude")
	if err != nil {
		t.Fatal(err)
	}
	session.mu.Lock()
	session.sharedID = "isolated-session"
	session.cliStartupDone = make(chan struct{})
	session.mu.Unlock()
	server := httptest.NewServer(s.handler())
	defer server.Close()
	s.origin = server.URL
	readState := func(ws *websocket.Conn, pending bool) {
		t.Helper()
		_ = ws.SetReadDeadline(time.Now().Add(10 * time.Second))
		kind, data, err := ws.ReadMessage()
		var state struct {
			Type    string `json:"type"`
			Pending bool   `json:"pending"`
		}
		if err != nil || kind != websocket.TextMessage || json.Unmarshal(data, &state) != nil || state.Type != "cli-startup" || state.Pending != pending {
			t.Fatalf("startup state: kind=%d data=%s err=%v", kind, data, err)
		}
	}
	first := connectTerminalViewer(t, s, server, session.id)
	readState(first, true)
	readTerminalViewerUntil(t, first, "terminal-ready")
	session.finishCLIStartup()
	readState(first, false)
	if err := first.WriteJSON(map[string]string{"type": "input", "data": terminalInputLine("after-startup")}); err != nil {
		t.Fatal(err)
	}
	readTerminalViewerUntil(t, first, "child:after-startup")
	_ = first.Close()
	second := connectTerminalViewer(t, s, server, session.id)
	readState(second, false)
	readTerminalViewerUntil(t, second, "child:after-startup")
	if session.summary()["cli_starting"] != false {
		t.Fatal("completed startup remained pending")
	}
}

func connectTerminalViewer(t *testing.T, s *Server, server *httptest.Server, id string) *websocket.Conn {
	t.Helper()
	headers := http.Header{"Origin": {server.URL}, "Cookie": {s.cookieName() + "=" + s.session}}
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/api/terminals/"+id+"/stream", headers)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Close() })
	return ws
}

func readTerminalViewerUntil(t *testing.T, ws *websocket.Conn, needle string) string {
	t.Helper()
	_ = ws.SetReadDeadline(time.Now().Add(10 * time.Second))
	var text string
	for !strings.Contains(text, needle) {
		_, chunk, err := ws.ReadMessage()
		if err != nil {
			t.Fatalf("viewer closed before %q: %v output=%q", needle, err, text)
		}
		text += string(chunk)
	}
	return text
}

func TestTerminalViewerDisconnectJoinsSenderAndKeepsPTY(t *testing.T) {
	s := testServer(t)
	t.Cleanup(s.terminals.close)
	session, err := s.terminals.start(terminalChildCommand("echo"), "/project", "shell")
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan struct{}, 2)
	handler := s.handler()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
		finished <- struct{}{}
	}))
	defer server.Close()
	s.origin = server.URL
	first := connectTerminalViewer(t, s, server, session.id)
	readTerminalViewerUntil(t, first, "terminal-ready")
	_ = first.Close()
	awaitTerminalSignal(t, finished)
	session.mu.Lock()
	clients, closed := len(session.clients), session.closed
	session.mu.Unlock()
	if clients != 0 || closed {
		t.Fatalf("viewer left subscription or ended process: clients=%d closed=%t", clients, closed)
	}
	second := connectTerminalViewer(t, s, server, session.id)
	if err := second.WriteJSON(map[string]string{"type": "input", "data": terminalInputLine("reconnected")}); err != nil {
		t.Fatal(err)
	}
	text := readTerminalViewerUntil(t, second, "child:reconnected")
	if !strings.Contains(text, "terminal-ready") {
		t.Fatal("reconnected viewer lost history")
	}
	s.terminals.remove(session.id)
	awaitTerminalSignal(t, session.done)
	awaitTerminalSignal(t, finished)
}

func TestTerminalHistoryIsMarkedBeforeClipboardReplay(t *testing.T) {
	s := testServer(t)
	t.Cleanup(s.terminals.close)
	history := []byte("\x1b]52;c;Y29weQ==\x07previous output")
	s.terminals.sessions = map[string]*terminalSession{"history": {id: "history", history: history, closed: true}}
	server := httptest.NewServer(s.handler())
	defer server.Close()
	s.origin = server.URL
	ws := connectTerminalViewer(t, s, server, "history")
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	var marker struct {
		Type string `json:"type"`
	}
	if err := ws.ReadJSON(&marker); err != nil || marker.Type != "history" {
		t.Fatalf("history marker: %+v, %v", marker, err)
	}
	kind, data, err := ws.ReadMessage()
	if err != nil || kind != websocket.BinaryMessage || string(data) != string(history) {
		t.Fatalf("history bytes changed: kind=%d data=%q err=%v", kind, data, err)
	}
}

type blockedInputTerminal struct {
	*controlledTerminal
	inputStarted chan struct{}
	inputClosed  chan struct{}
	inputOnce    sync.Once
	closeOnce    sync.Once
}

func (f *blockedInputTerminal) Write([]byte) (int, error) {
	f.inputOnce.Do(func() { close(f.inputStarted) })
	<-f.inputClosed
	return 0, os.ErrClosed
}
func (f *blockedInputTerminal) Close() error {
	f.closeOnce.Do(func() { close(f.inputClosed) })
	return f.controlledTerminal.Close()
}

func TestServiceCloseReleasesTerminalHandlerBlockedInPTYWrite(t *testing.T) {
	s := testServer(t)
	reader, writer := io.Pipe()
	file := &blockedInputTerminal{controlledTerminal: &controlledTerminal{reader: reader, writer: writer, killed: make(chan struct{}), waitStarted: make(chan struct{}), waitRelease: make(chan struct{})}, inputStarted: make(chan struct{}), inputClosed: make(chan struct{})}
	session := newTerminalSession(file)
	session.id = "blocked"
	s.terminals.sessions = map[string]*terminalSession{session.id: session}
	go session.run()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(file.waitRelease) }) }
	t.Cleanup(func() { release(); s.terminals.close() })
	finished := make(chan struct{})
	handler := s.handler()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(finished)
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	s.origin = server.URL
	ws := connectTerminalViewer(t, s, server, session.id)
	if err := ws.WriteJSON(map[string]string{"type": "input", "data": "blocked"}); err != nil {
		t.Fatal(err)
	}
	awaitTerminalSignal(t, file.inputStarted)
	release()
	serviceClosed := make(chan struct{})
	go func() { _ = s.closeService(); close(serviceClosed) }()
	awaitTerminalSignal(t, serviceClosed)
	awaitTerminalSignal(t, finished)
	awaitTerminalSignal(t, session.done)
	session.mu.Lock()
	clients := len(session.clients)
	session.mu.Unlock()
	if clients != 0 {
		t.Fatal("service exited with attached terminal subscriber")
	}
}

func TestBlockedTerminalInputDoesNotRetainDisconnectedViewer(t *testing.T) {
	for _, action := range []string{"disconnect", "account-lock", "full-queue-disconnect", "full-queue-lock"} {
		t.Run(action, func(t *testing.T) {
			s := testServer(t)
			reader, writer := io.Pipe()
			file := &blockedInputTerminal{controlledTerminal: &controlledTerminal{reader: reader, writer: writer, killed: make(chan struct{}), waitStarted: make(chan struct{}), waitRelease: make(chan struct{})}, inputStarted: make(chan struct{}), inputClosed: make(chan struct{})}
			session := newTerminalSession(file)
			session.id = "blocked"
			s.terminals.sessions = map[string]*terminalSession{session.id: session}
			go session.run()
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(file.waitRelease) }) }
			t.Cleanup(func() { release(); s.terminals.close() })
			finished := make(chan struct{})
			handler := s.handler()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(finished)
				handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			s.origin = server.URL
			ws := connectTerminalViewer(t, s, server, session.id)
			if err := ws.WriteJSON(map[string]string{"type": "input", "data": "blocked"}); err != nil {
				t.Fatal(err)
			}
			awaitTerminalSignal(t, file.inputStarted)
			if strings.HasPrefix(action, "full-queue") {
				// One message is blocked in the PTY; the next eight fill the
				// session queue. The last must not retain this HTTP handler.
				for i := 0; i < 9; i++ {
					if err := ws.WriteJSON(map[string]string{"type": "input", "data": "queued"}); err != nil {
						t.Fatal(err)
					}
				}
			}
			if strings.HasSuffix(action, "lock") {
				s.account.lock()
			} else {
				_ = ws.Close()
			}
			awaitTerminalSignal(t, finished)
			session.mu.Lock()
			clients, closed := len(session.clients), session.closed
			session.mu.Unlock()
			if clients != 0 || closed {
				t.Fatalf("viewer cleanup retained a subscription or killed PTY: clients=%d closed=%t", clients, closed)
			}
			select {
			case <-file.killed:
				t.Fatal("viewer cleanup killed the background process")
			default:
			}
			select {
			case <-session.done:
				t.Fatal("viewer cleanup ended the session")
			default:
			}
			release()
			serviceClosed := make(chan struct{})
			go func() { _ = s.closeService(); close(serviceClosed) }()
			awaitTerminalSignal(t, serviceClosed)
			awaitTerminalSignal(t, session.done)
		})
	}
}
