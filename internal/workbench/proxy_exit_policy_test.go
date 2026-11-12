package workbench

import (
	"context"
	"errors"
	"testing"

	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/managedproxy"
)

func TestProxySelectionKeepsExitBindingsWithTheirNodes(t *testing.T) {
	s := testServer(t)
	p, _ := managedproxy.Load(s.profile)
	first := p.Selected
	if err := p.Import("other", "", []byte(`proxies: [{name: other, type: socks5, server: 127.0.0.1, port: 2}]`), "other", 0, ""); err != nil {
		t.Fatal(err)
	}
	second := p.Sources[1].Nodes[0].ID
	if err := p.Save(s.profile); err != nil {
		t.Fatal(err)
	}
	cfg, _ := loadConfig(s.configPath)
	// A legacy global anchor has no proven relationship to the selected node.
	cfg.ExitPolicy = cage.ExitPolicy{Mode: "static", Scope: "country", IP: "8.8.8.8"}
	if err := writePrivate(s.configPath, cfg); err != nil {
		t.Fatal(err)
	}
	validate, start := validateProxyCore, startProxyCore
	t.Cleanup(func() { validateProxyCore, startProxyCore = validate, start })
	validateProxyCore = func(context.Context, string, managedproxy.Store) error { return nil }
	fail := false
	startProxyCore = func(context.Context, string, managedproxy.Store) error {
		if fail {
			return errors.New("core startup failed")
		}
		return nil
	}
	selectNode := func(id string) cage.Config {
		t.Helper()
		if err := s.changeProxy("select", proxyInput{NodeID: id}, nil); err != nil {
			t.Fatal(err)
		}
		got, err := loadConfig(s.configPath)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	bind := func(ip string) {
		t.Helper()
		c, _ := loadConfig(s.configPath)
		c.ExitPolicy = c.ExitPolicy.BindMissing(cage.ExitLocation{IP: ip, CountryCode: "US"})
		if err := s.saveExitPolicyConfig(c); err != nil {
			t.Fatal(err)
		}
	}
	if got := selectNode(first); got.ExitPolicy.IP != "" || got.ExitPolicy.Mode != "static" {
		t.Fatalf("first explicit selection inherited legacy global IP: %+v", got.ExitPolicy)
	}
	bind("1.1.1.1")
	if got := selectNode(second); got.ExitPolicy.IP != "" || got.ExitPolicy.Mode != "static" {
		t.Fatalf("new exit inherited another node's IP: %+v", got.ExitPolicy)
	}
	bind("8.8.4.4")
	if got := selectNode(first); got.ExitPolicy.IP != "1.1.1.1" {
		t.Fatalf("return to first exit lost binding: %+v", got.ExitPolicy)
	}
	if err := s.changeProxy("route", proxyInput{NodeIDs: []string{second, first}}, nil); err != nil {
		t.Fatal(err)
	}
	cfg, _ = loadConfig(s.configPath)
	if cfg.ExitPolicy.IP != "1.1.1.1" {
		t.Fatal("transit change reset final exit binding")
	}
	if err := s.changeProxy("restart", proxyInput{}, nil); err != nil {
		t.Fatal(err)
	}
	cfg, _ = loadConfig(s.configPath)
	if cfg.ExitPolicy.IP != "1.1.1.1" {
		t.Fatal("core restart reset exit binding")
	}
	fail = true
	if err := s.changeProxy("select", proxyInput{NodeID: second}, nil); err == nil {
		t.Fatal("failed startup accepted")
	}
	cfg, _ = loadConfig(s.configPath)
	saved, _ := managedproxy.Load(s.profile)
	if saved.Selected != first || cfg.ExitPolicy.IP != "1.1.1.1" {
		t.Fatal("failed switch replaced working exit or binding")
	}
	fail = false
	if got := selectNode(second); got.ExitPolicy.IP != "8.8.4.4" {
		t.Fatalf("second binding was lost: %+v", got.ExitPolicy)
	}
	loc := cage.ExitLocation{IP: "9.9.9.9", CountryCode: "US"}
	cfg, _ = loadConfig(s.configPath)
	if state, _ := cfg.ExitPolicy.Evaluate(loc.IP, &loc); state != cage.CheckFailed {
		t.Fatal("same-node IP drift was accepted")
	}
	cfg.ExitPolicy.IP = "9.9.9.9"
	if err := s.apply(savedProfile{Config: cfg, Preferences: s.preferences}, false); err != nil {
		t.Fatal(err)
	}
	selectNode(first)
	if got := selectNode(second); got.ExitPolicy.IP != "9.9.9.9" {
		t.Fatal("settings edit did not follow the final exit node")
	}
}
