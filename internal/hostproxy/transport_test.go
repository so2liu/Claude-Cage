package hostproxy

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http/httpproxy"
)

func TestHTTPSDownloadUsesHostProxyAndRedirects(t *testing.T) {
	var connects atomic.Int32
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/first" {
			http.Redirect(w, r, "/image", http.StatusFound)
			return
		}
		io.WriteString(w, "image bytes")
	}))
	defer origin.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "CONNECT" || r.Host != "image.invalid:443" {
			t.Errorf("unexpected proxy request: %s %s", r.Method, r.Host)
			http.Error(w, "bad target", 400)
			return
		}
		connects.Add(1)
		upstream, err := net.Dial("tcp", strings.TrimPrefix(origin.URL, "https://"))
		if err != nil {
			t.Error(err)
			return
		}
		defer upstream.Close()
		downstream, buffer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer downstream.Close()
		buffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		buffer.Flush()
		go io.Copy(upstream, buffer)
		io.Copy(downstream, upstream)
	}))
	defer proxy.Close()
	proxyURL, _ := url.Parse(proxy.URL)
	tr := transport(&httpproxy.Config{}, func(*http.Request) (*url.URL, error) { return proxyURL, nil })
	tr.TLSClientConfig = origin.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	// The simulated official domain is intentionally unresolvable. TLS still
	// checks the fixture certificate using its original name.
	tr.TLSClientConfig.ServerName = "example.com"
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	resp, err := client.Get("https://image.invalid/first")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "image bytes" || connects.Load() == 0 {
		t.Fatalf("body=%q proxy CONNECTs=%d", body, connects.Load())
	}
}

func TestHostProxyFailureNeverFallsBackToOrigin(t *testing.T) {
	var direct atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { direct.Add(1) }))
	defer origin.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead, _ := url.Parse("http://" + listener.Addr().String())
	listener.Close()
	for _, resolve := range []func(*http.Request) (*url.URL, error){
		func(*http.Request) (*url.URL, error) { return dead, nil },
		func(*http.Request) (*url.URL, error) { return nil, errors.New("PAC failed") },
	} {
		tr := transport(&httpproxy.Config{}, resolve)
		_, err := (&http.Client{Transport: tr, Timeout: time.Second}).Get(origin.URL)
		tr.CloseIdleConnections()
		if err == nil {
			t.Fatal("proxy failure accepted")
		}
	}
	if direct.Load() != 0 {
		t.Fatal("origin contacted after proxy failure")
	}
}

func TestEnvironmentOverridesAndBypass(t *testing.T) {
	for _, test := range []struct{ address, want string }{{"https://official.invalid/image", "http://127.0.0.1:7890"}, {"https://skip.invalid/image", ""}} {
		tr := transport(&httpproxy.Config{HTTPSProxy: "http://127.0.0.1:7890", NoProxy: "skip.invalid"}, func(*http.Request) (*url.URL, error) { t.Fatal("system proxy used over explicit env"); return nil, nil })
		req, _ := http.NewRequest("GET", test.address, nil)
		got, err := tr.Proxy(req)
		if err != nil {
			t.Fatal(err)
		}
		value := ""
		if got != nil {
			value = got.String()
		}
		if value != test.want {
			t.Fatalf("got %q want %q", value, test.want)
		}
	}
}

func TestHTTPOnlyEnvironmentKeepsHTTPSSystemProxy(t *testing.T) {
	want, _ := url.Parse("http://127.0.0.1:7896")
	tr := transport(&httpproxy.Config{HTTPProxy: "http://127.0.0.1:7890"}, func(*http.Request) (*url.URL, error) { return want, nil })
	req, _ := http.NewRequest("GET", "https://official.invalid/image", nil)
	got, err := tr.Proxy(req)
	if err != nil || got == nil || got.String() != want.String() {
		t.Fatalf("proxy=%v err=%v", got, err)
	}
}

func TestHostDownloadSubprocessKeepsCapturedProxyAfterGuestSanitization(t *testing.T) {
	environment.Lock()
	previous := environment.config
	environment.config = nil
	environment.Unlock()
	t.Cleanup(func() { environment.Lock(); environment.config = previous; environment.Unlock() })
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:7896")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:7896")
	t.Setenv("NO_PROXY", "skip.invalid")
	t.Setenv("no_proxy", "skip.invalid")
	t.Setenv("https_proxy", "")
	t.Setenv("http_proxy", "")
	CaptureEnvironment()
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	got := strings.Join(DownloadEnvironment(), "\n")
	if !strings.Contains(got, "HTTPS_PROXY=http://127.0.0.1:7896") || !strings.Contains(got, "NO_PROXY=skip.invalid") {
		t.Fatalf("host subprocess lost captured proxy settings: %s", got)
	}
	if os.Getenv("HTTPS_PROXY") != "" {
		t.Fatal("host settings were restored into the guest process environment")
	}
}

// Run manually to verify the real system proxy without changing host settings.
func TestLiveSystemProxyDownload(t *testing.T) {
	if testing.Short() {
		t.Skip("live check")
	}
	// Opt-in keeps ordinary tests independent of the developer's network.
	if os.Getenv("CAGE_TEST_HOST_PROXY_LIVE") == "" {
		t.Skip("set CAGE_TEST_HOST_PROXY_LIVE=1")
	}
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy", "NO_PROXY", "no_proxy"} {
		t.Setenv(name, "")
	}
	tr := NewTransport()
	defer tr.CloseIdleConnections()
	req, _ := http.NewRequestWithContext(context.Background(), "GET", "https://cloud.debian.org/images/cloud/bookworm/latest/SHA512SUMS", nil)
	proxy, err := tr.Proxy(req)
	if err != nil || proxy == nil {
		t.Fatalf("system proxy=%v err=%v", proxy, err)
	}
	var peer string
	dial := tr.DialContext
	tr.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		peer = address
		return dial(ctx, network, address)
	}
	resp, err := (&http.Client{Transport: tr, Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if peer != proxy.Host || resp.StatusCode != 200 || len(line) < 128 {
		t.Fatalf("peer=%s proxy=%s status=%d bytes=%d", peer, proxy.Host, resp.StatusCode, len(line))
	}
	t.Logf("system proxy %s, connected peer %s, official Debian checksum HTTP %d (%d-byte first line)", proxy.Redacted(), peer, resp.StatusCode, len(line))
	if image := os.Getenv("CAGE_TEST_HOST_IMAGE_URL"); image != "" {
		req, _ := http.NewRequest("GET", image, nil)
		req.Header.Set("Range", "bytes=0-65535")
		resp, err := (&http.Client{Transport: tr, Timeout: 30 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		count, err := io.Copy(io.Discard, io.LimitReader(resp.Body, 65537))
		if err != nil || resp.StatusCode != 206 || count != 65536 || peer != proxy.Host {
			t.Fatalf("image peer=%s status=%d bytes=%d err=%v", peer, resp.StatusCode, count, err)
		}
		t.Logf("official system image HTTP %d, received %d bytes through %s", resp.StatusCode, count, peer)
	}
}
