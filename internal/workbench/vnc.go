package workbench

import (
	"context"
	"github.com/gorilla/websocket"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sylvinus/agent-vm/internal/containerenv"
	"github.com/sylvinus/agent-vm/internal/diagnostics"
)

func (s *Server) vnc(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	ready := s.ready
	s.mu.Unlock()
	if !ready {
		http.Error(w, "环境尚未就绪", 409)
		return
	}
	if s.macosGuest != nil || s.windowsGuest != nil {
		s.nativeBrowserVNC(w, r)
		return
	}
	release, err := s.browserViews.acquire(s.changeBrowserVisibility)
	if err != nil {
		requestError(w, err)
		return
	}
	defer release()
	address := "127.0.0.1:15900"
	if containerenv.Configured(s.profile) {
		environment, err := containerenv.Load(s.profile)
		if err != nil {
			requestError(w, err)
			return
		}
		state, err := environment.State(r.Context())
		if err != nil || !state.Ready {
			http.Error(w, "容器画面连接尚未就绪", 409)
			return
		}
		address = state.VNCAddress
	}
	remote, e := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(r.Context(), "tcp", address)
	if e != nil {
		http.Error(w, "画面连接暂时不可用", 502)
		return
	}
	defer remote.Close()
	s.streamVNC(w, r, remote, remote, func() { _ = remote.Close() })
}

func (s *Server) streamVNC(w http.ResponseWriter, r *http.Request, reader io.Reader, writer io.Writer, closeTransport func()) {
	var once sync.Once
	closeRemote := func() { once.Do(closeTransport) }
	defer closeRemote()
	ws, e := s.upgrade(w, r)
	if e != nil {
		return
	}
	defer ws.Close()
	ws.SetReadLimit(4 << 20)
	closeStreams := func() { _ = ws.Close(); closeRemote() }
	stop := context.AfterFunc(r.Context(), closeStreams)
	done := make(chan struct{})
	// A menu capture can briefly pause the guest while pointer events continue.
	// Bound both count and bytes; four tiny mouse packets must not tear down a
	// healthy display. No protocol bytes are discarded or reordered.
	input := make(chan []byte, 64)
	var queuedBytes atomic.Int64
	written := make(chan struct{})
	defer func() {
		stop()
		closeStreams()
		close(input)
		<-done
		<-written
	}()
	// Keep reading WebSocket close/control frames even while the guest stops
	// accepting input. Overflow closes this display connection without dropping
	// bytes into an otherwise live RFB stream.
	go func() {
		defer close(written)
		defer closeStreams()
		for data := range input {
			if _, err := writer.Write(data); err != nil {
				return
			}
			queuedBytes.Add(-int64(len(data)))
		}
	}()
	go func() {
		defer close(done)
		defer ws.Close()
		b := make([]byte, 64<<10)
		for {
			n, e := reader.Read(b)
			if n > 0 {
				_ = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
				if ws.WriteMessage(websocket.BinaryMessage, b[:n]) != nil {
					return
				}
			}
			if e != nil {
				return
			}
		}
	}()
	for {
		kind, b, e := ws.ReadMessage()
		if e != nil {
			return
		}
		if kind == websocket.BinaryMessage {
			if queuedBytes.Add(int64(len(b))) > 8<<20 {
				diagnostics.Log("warn", "display-transport", "input byte limit reached")
				return
			}
			// Allow short input bursts to drain. A stalled guest still has a
			// bounded queue and cannot hide a client disconnect indefinitely.
			timer := time.NewTimer(time.Second)
			select {
			case input <- b:
				timer.Stop()
			case <-r.Context().Done():
				timer.Stop()
				return
			case <-timer.C:
				diagnostics.Log("warn", "display-transport", "input stalled: bounded queue did not drain")
				return
			}
		}
	}
}
