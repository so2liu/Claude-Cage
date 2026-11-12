package cage

import (
	"context"
	"encoding/json"
	"github.com/sylvinus/agent-vm/internal/managedproxy"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestProxyRevisionRevokesOldReportAtSameEndpoint(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ProxyRevision = "first"
	g, err := NewEgressGate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close("done")
	if err = g.BeginCheck(); err != nil {
		t.Fatal(err)
	}
	state := g.Status()
	report := SafetyReport{Policy: SafetyPolicyVersion, ID: "old-report", RunID: state.RunID, CheckID: state.CheckID, Configuration: cfg.Fingerprint(), CreatedAt: time.Now()}
	for _, id := range requiredSafetyChecks {
		report.Checks = append(report.Checks, SafetyCheck{ID: id, State: CheckPassed, Critical: true, Evidence: "test", CheckedAt: time.Now()})
	}
	if err = g.Grant(report); err != nil {
		t.Fatal(err)
	}
	next := cfg
	next.ProxyRevision = "second"
	next.ExitPolicy = ExitPolicy{Mode: "static", IP: "1.1.1.1"}
	if err = g.SyncProxyRevision(next); err != nil {
		t.Fatal(err)
	}
	state = g.Status()
	if state.Mode != GateClosed || state.Report != nil || state.Configuration != next.Fingerprint() {
		t.Fatal("old generation retained")
	}
	if err = g.Renew(report.ID); err == nil {
		t.Fatal("old lease renewed")
	}
	if err = g.BeginCheck(); err != nil {
		t.Fatal(err)
	}
	if err = g.Grant(report); err == nil {
		t.Fatal("old report authorized a new node")
	}
	changed := next
	changed.ExitPolicy.IP = "8.8.8.8"
	if err = g.SyncProxyRevision(changed); err == nil {
		t.Fatal("same proxy generation silently replaced its bound IP")
	}
	changed = next
	changed.ProxyURL = "socks5://127.0.0.1:3333"
	if err = g.SyncProxyRevision(changed); err == nil {
		t.Fatal("transport changed without environment restart")
	}
}

func TestProxyRevisionControlUpdatesHostRunningRecord(t *testing.T) {
	profile := t.TempDir()
	path := filepath.Join(profile, "config.json")
	state := filepath.Join(profile, "state")
	instance := filepath.Join(state, "lima", "qa")
	if err := os.MkdirAll(instance, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.ProxyRevision = "initial"
	p, _ := managedproxy.Load(profile)
	if err := p.Import("qa", "", []byte(`proxies: [{name: qa, type: socks5, server: 127.0.0.1, port: 1}]`), "qa", 0, ""); err != nil {
		t.Fatal(err)
	}
	p.Selected = p.Sources[0].Nodes[0].ID
	p.Revision = cfg.ProxyRevision
	cfg.ProxyURL = p.Endpoint()
	if err := p.Save(profile); err != nil {
		t.Fatal(err)
	}
	write := func(c Config) {
		b, _ := json.Marshal(c)
		if err := os.WriteFile(path, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(cfg)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	g, err := NewEgressGate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	stop, err := StartGateControl(path, instance, g)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	client, err := OpenGateClient(instance)
	if err != nil {
		t.Fatal(err)
	}
	next := cfg
	next.ProxyRevision = "new-core"
	p.Revision = next.ProxyRevision
	if err := p.Save(profile); err != nil {
		t.Fatal(err)
	}
	write(next)
	status, err := client.SyncProxyRevision(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Configuration != next.Fingerprint() || status.Mode != GateClosed || status.Report != nil {
		t.Fatal("control did not revoke old generation")
	}
	if err = next.CheckRunning(state); err != nil {
		t.Fatal("running record did not adopt node generation", err)
	}
	if err = cfg.CheckRunning(state); err == nil {
		t.Fatal("old configuration still matches running record")
	}
}

func TestProxyRevisionCompletesOnlyMissingExitAnchor(t *testing.T) {
	for _, policy := range []ExitPolicy{
		{Mode: "static", Scope: "country"},
		{Mode: "dynamic", Scope: "city", CountryCode: "US"},
	} {
		cfg := DefaultConfig()
		cfg.ProxyRevision = "same-node"
		cfg.ExitPolicy = policy
		gate, err := NewEgressGate(cfg)
		if err != nil {
			t.Fatal(err)
		}
		next := cfg
		next.ExitPolicy = policy.BindMissing(ExitLocation{IP: "8.8.8.8", CountryCode: "US", Region: "California", City: "Mountain View"})
		if err := gate.SyncProxyRevision(next); err != nil {
			t.Fatal("first binding required a VM restart", err)
		}
		if got := gate.Status(); got.Mode != GateClosed || got.Configuration != next.Fingerprint() || got.Report != nil {
			t.Fatal("first binding reused authorization")
		}
		changed := next
		changed.ExitPolicy.IP = "1.1.1.1"
		changed.ExitPolicy.CountryCode = "JP"
		if err := gate.SyncProxyRevision(changed); err == nil {
			t.Fatal("completed binding accepted a replacement anchor")
		}
		gate.Close("done")
	}
	cfg := DefaultConfig()
	cfg.ProxyRevision = "same-node"
	cfg.ExitPolicy = ExitPolicy{Mode: "dynamic", Scope: "city", CountryCode: "US"}
	gate, err := NewEgressGate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Close("done")
	cfg.ExitPolicy.CountryCode = "JP"
	cfg.ExitPolicy.Region = "Tokyo"
	cfg.ExitPolicy.City = "Tokyo"
	if err := gate.SyncProxyRevision(cfg); err == nil {
		t.Fatal("partial binding overwrote an existing country")
	}
}
