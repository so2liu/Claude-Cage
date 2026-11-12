package cage

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/diagnostics"
)

func TestExitProbeBodyTimeoutKeepsDeadlineAndShortReadCause(t *testing.T) {
	for _, stall := range []bool{false, true} {
		name := "incomplete-body"
		if stall {
			name = "body-timeout"
		}
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", "100")
				_, _ = io.WriteString(w, "8.8.8.8")
				w.(http.Flusher).Flush()
				if stall {
					<-r.Context().Done()
				}
			}))
			defer server.Close()
			client := &http.Client{Timeout: 100 * time.Millisecond}
			ip, err := queryExitIP(context.Background(), client, server.URL)
			want := error(io.ErrUnexpectedEOF)
			if stall {
				want = context.DeadlineExceeded
			}
			if ip != "" || !errors.Is(err, want) || strings.Contains(err.Error(), server.URL) {
				t.Fatalf("body error lost its safe cause: ip=%q error=%v", ip, err)
			}
		})
	}
}

func TestExitProbeProxyFailureKeepsServiceAndHTTPStatusAfterRedaction(t *testing.T) {
	profile := t.TempDir()
	store := diagnostics.New(profile)
	previous := diagnostics.Default()
	diagnostics.SetDefault(store)
	t.Cleanup(func() { diagnostics.SetDefault(previous) })
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusProxyAuthRequired)
		_, _ = io.WriteString(w, "private-response")
	}))
	defer proxy.Close()
	cfg := DefaultConfig()
	cfg.ProxyURL = strings.Replace(proxy.URL, "://", "://private-user:private-password@", 1)
	ip, err := ProbeExitIP(context.Background(), cfg)
	if ip != "" || err == nil || !strings.Contains(err.Error(), "HTTP 407") || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("proxy rejection was hidden or treated as a timeout: %s %v", ip, err)
	}
	log, readErr := os.ReadFile(filepath.Join(profile, "logs", "events.jsonl"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	for _, marker := range []string{"service=icanhazip", "service=ipify", "duration_ms=", "HTTP 407"} {
		if !strings.Contains(string(log), marker) {
			t.Fatalf("missing diagnostic %q: %s", marker, log)
		}
	}
	for _, private := range []string{"private-response", "private-user", "private-password", proxy.URL} {
		if strings.Contains(err.Error(), private) || strings.Contains(string(log), private) {
			t.Fatalf("private proxy data leaked: %v %s", err, log)
		}
	}
}
