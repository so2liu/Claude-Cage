package guestos

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMacOSMediaEnvironment(t *testing.T) {
	parent := []string{"PATH=/usr/bin:/bin", "HTTPS_PROXY=http://wrong.invalid:1", "http_proxy=http://wrong.invalid:2", "NO_PROXY=*", "no_proxy=*", "ALL_PROXY=wrong", "CAGE_REFRESH_TOOLS=1"}
	env := macOSMediaEnvironment(parent, "socks5://127.0.0.1:17896", false)
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "wrong") || strings.Contains(joined, "NO_PROXY") || strings.Contains(joined, "no_proxy") || strings.Contains(joined, "CAGE_REFRESH_TOOLS") {
		t.Fatalf("inherited proxy bypass or refresh flag: %s", joined)
	}
	if !strings.Contains(joined, "https_proxy=socks5h://127.0.0.1:17896") || !strings.Contains(joined, "PATH=/usr/bin:/bin") {
		t.Fatalf("missing proxy DNS or ordinary environment: %s", joined)
	}
	if !strings.Contains(strings.Join(macOSMediaEnvironment(nil, "http://127.0.0.1:17896", true), "\n"), "CAGE_REFRESH_TOOLS=1") {
		t.Fatal("tool refresh was not forwarded")
	}
}

func TestPrepareMacOSMediaCancellationStopsDescendants(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("macOS downloader runs a shell")
	}
	kit, cache := t.TempDir(), t.TempDir()
	script := "#!/bin/bash\n(sleep 1; touch \"$1/continued-after-stop\") &\ntouch \"$1/started\"\nwait\n"
	if err := os.WriteFile(filepath.Join(kit, "prepare-macos-media.sh"), []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- prepareMacOSMedia(ctx, kit, cache, "socks5://127.0.0.1:17896", false, io.Discard, io.Discard)
	}()
	deadline := time.After(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(cache, "started")); err == nil {
			break
		}
		select {
		case <-deadline:
			t.Fatal("downloader did not start")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("cancelled downloader succeeded")
	}
	time.Sleep(1100 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(cache, "continued-after-stop")); !os.IsNotExist(err) {
		t.Fatal("download descendant continued after cancellation")
	}
}

func TestPrepareMacOSMediaUsesSelectedProxyAndCannotBypassIt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("macOS downloader runs a shell")
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl unavailable")
	}
	kit, cache := t.TempDir(), t.TempDir()
	var proxied atomic.Bool
	var direct atomic.Bool
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		direct.Store(true)
		_, _ = io.WriteString(w, "direct-leak")
	}))
	defer origin.Close()
	originURL, _ := url.Parse(origin.URL)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host != originURL.Host {
			t.Errorf("unexpected source: %s", r.URL.Host)
		}
		proxied.Store(true)
		_, _ = io.WriteString(w, "verified-fixture")
	}))
	defer proxy.Close()
	script := "#!/bin/bash\nset -eu\ncurl --disable --fail --silent --show-error --max-time 3 " + origin.URL + "/package -o \"$1/result\"\n"
	if err := os.WriteFile(filepath.Join(kit, "prepare-macos-media.sh"), []byte(script), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NO_PROXY", "*")
	t.Setenv("no_proxy", "*")
	var output bytes.Buffer
	if err := prepareMacOSMedia(t.Context(), kit, cache, proxy.URL, false, &output, &output); err != nil {
		t.Fatalf("download failed: %v %s", err, output.String())
	}
	got, err := os.ReadFile(filepath.Join(cache, "result"))
	if err != nil || string(got) != "verified-fixture" || !proxied.Load() {
		t.Fatalf("download bypassed selected proxy: %q %v", got, err)
	}
	proxy.Close()
	if err := prepareMacOSMedia(t.Context(), kit, cache, proxy.URL, false, &output, &output); err == nil {
		t.Fatal("unavailable selected proxy did not fail closed")
	}
	if direct.Load() {
		t.Fatal("download bypassed selected proxy and reached the origin directly")
	}
}
