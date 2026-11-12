package workbench

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/cage"
)

func TestExitProbeTimeoutKeepsLiveTunnelUntilManualPause(t *testing.T) {
	// The loopback proxy echoes a synthetic public CONNECT target. No public
	// network or application dialer is used by the fixture's transport.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "CONNECT" || r.Host != "8.8.8.8:443" {
			w.WriteHeader(400)
			return
		}
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = rw.Flush()
		_, _ = io.Copy(conn, rw)
	}))
	defer proxy.Close()
	s := testServer(t)
	cfg, err := loadConfig(s.configPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ProxyURL = proxy.URL
	if err := writePrivate(s.configPath, cfg); err != nil {
		t.Fatal(err)
	}
	gate, err := cage.NewEgressGate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { gate.Close("test complete") })
	if err := os.MkdirAll(s.instanceDir(), 0700); err != nil {
		t.Fatal(err)
	}
	stop, err := cage.StartGateControl(s.configPath, s.instanceDir(), gate)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	if err := gate.BeginCheck(); err != nil {
		t.Fatal(err)
	}
	grantTestGate(t, gate)
	s.publishGate(gate.Status())
	s.ready = true
	s.safety.VerifiedAt = time.Now().Add(-time.Minute)
	before := gate.Status()
	conn, err := gate.DialContext(t.Context(), "tcp", "8.8.8.8:443")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	echo := func(message string) {
		t.Helper()
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		if _, err := io.WriteString(conn, message); err != nil {
			t.Fatal(err)
		}
		buffer := make([]byte, len(message))
		if _, err := io.ReadFull(conn, buffer); err != nil || string(buffer) != message {
			t.Fatalf("tunnel roundtrip failed: %q %v", buffer, err)
		}
	}
	echo("before timeout")
	s.monitorSafetyWithProbe(func(context.Context, cage.Config) (string, error) { return "", context.DeadlineExceeded })
	if after := gate.Status(); after.Mode != cage.GateOpen || !after.Until.Equal(before.Until) || after.Epoch != before.Epoch {
		t.Fatalf("sampling timeout changed permission: before=%+v after=%+v", before, after)
	}
	echo("after timeout")
	if !strings.Contains(s.currentNetworkErrorLocked(), "超时") {
		t.Fatal("sampling timeout was not visible")
	}
	if err := s.pauseNetworkNow("手动暂停"); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(conn, "after pause"); err == nil {
		t.Fatal("manual pause kept the established tunnel alive")
	}
	if next, err := gate.DialContext(t.Context(), "tcp", "8.8.8.8:443"); err == nil {
		next.Close()
		t.Fatal("paused gate accepted a new tunnel")
	}
	if gate.Renew(before.Report.ID) == nil {
		t.Fatal("old report reopened a paused gate")
	}
}

func TestNetworkRenewalRunsWhileOperationLockIsHeld(t *testing.T) {
	s, gate := grantedToolUpdateServer(t)
	s.setJobForTest(Job{})
	client, err := s.gateClient()
	if err != nil {
		t.Fatal(err)
	}
	before := gate.Status()
	// Advance beyond the grant's wall-clock tick on Windows as well.
	time.Sleep(5 * time.Millisecond)
	s.op.Lock()
	defer s.op.Unlock()
	finished := make(chan struct{})
	go func() { defer close(finished); s.finishSafetyMonitor(t.Context(), client, before, "8.8.8.8", "") }()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("lease renewal waited for the lifecycle operation lock")
	}
	if after := gate.Status(); after.Mode != cage.GateOpen || !after.Until.After(before.Until) {
		t.Fatalf("monitor did not renew its lease: before=%+v after=%+v", before, after)
	}
}
