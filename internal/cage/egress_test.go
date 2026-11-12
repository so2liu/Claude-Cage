package cage

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
)

func TestProxyHandshakeTimeoutPreservesDeadline(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(io.Discard, conn)
	}))
	defer proxy.Close()
	cfg := DefaultConfig()
	cfg.ProxyURL = proxy.URL
	e, err := NewEgress(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	conn, err := e.DialContext(ctx, "tcp", "8.8.8.8:443")
	if conn != nil {
		conn.Close()
		t.Fatal("stalled CONNECT succeeded")
	}
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "CONNECT") || !strings.Contains(err.Error(), "超时") {
		t.Fatalf("CONNECT timeout lost its cause or stage: %v", err)
	}
}

// This proxy records CONNECT targets and transports actual bytes. It does not
// use the application's dialer, so wrong wiring cannot pass unnoticed.
func testProxy(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	count := new(atomic.Int32)
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "CONNECT" {
			http.Error(w, "CONNECT required", 405)
			return
		}
		count.Add(1)
		target, err := net.DialTimeout("tcp", r.Host, time.Second)
		if err != nil {
			http.Error(w, "unreachable", 502)
			return
		}
		client, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			target.Close()
			return
		}
		rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		rw.Flush()
		go func() { defer client.Close(); defer target.Close(); io.Copy(target, rw) }()
		go func() { defer client.Close(); defer target.Close(); io.Copy(client, target) }()
	}))
	t.Cleanup(s.Close)
	return s, count
}

func TestCONNECTAndFailureDoesNotDialTarget(t *testing.T) {
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls.Add(1); io.WriteString(w, "through proxy") }))
	defer target.Close()
	p, count := testProxy(t)
	c := DefaultConfig()
	c.ProxyURL = p.URL
	e, err := NewEgress(c)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{DialContext: e.DialContext}, Timeout: time.Second}
	r, err := client.Get(target.URL)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, r.Body)
	r.Body.Close()
	if count.Load() != 1 || targetCalls.Load() != 1 {
		t.Fatal("request did not traverse proxy")
	}
	client.CloseIdleConnections()
	p.Close()
	if _, err = client.Get(target.URL); err == nil {
		t.Fatal("proxy outage did not fail")
	}
	if targetCalls.Load() != 1 {
		t.Fatal("request escaped directly during outage")
	}
}

func TestSOCKSUsesRemoteDNSAndAuthentication(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	done := make(chan string, 1)
	go func() {
		conn, err := l.Accept()
		if err != nil {
			done <- err.Error()
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(2 * time.Second))
		r := bufio.NewReader(conn)
		header := make([]byte, 2)
		io.ReadFull(r, header)
		methods := make([]byte, int(header[1]))
		io.ReadFull(r, methods)
		conn.Write([]byte{5, 2})
		io.ReadFull(r, header)
		user := make([]byte, int(header[1]))
		io.ReadFull(r, user)
		n, _ := r.ReadByte()
		pass := make([]byte, int(n))
		io.ReadFull(r, pass)
		if string(user) != "alice" || string(pass) != "secret" {
			done <- "auth mismatch"
			return
		}
		conn.Write([]byte{1, 0})
		request := make([]byte, 4)
		io.ReadFull(r, request)
		if request[3] != 3 {
			done <- "hostname was resolved locally"
			return
		}
		n, _ = r.ReadByte()
		host := make([]byte, int(n))
		io.ReadFull(r, host)
		port := make([]byte, 2)
		io.ReadFull(r, port)
		conn.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 80})
		done <- string(host)
	}()
	c := DefaultConfig()
	c.ProxyURL = "socks5://alice:secret@" + l.Addr().String()
	e, _ := NewEgress(c)
	conn, err := e.DialContext(context.Background(), "tcp", "does-not-exist.invalid:443")
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if got := <-done; got != "does-not-exist.invalid" {
		t.Fatal(got)
	}
}

func TestDNSUsesProxyAndNoFallback(t *testing.T) {
	var queries atomic.Int32
	doh := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries.Add(1)
		b, _ := io.ReadAll(r.Body)
		q := new(dns.Msg)
		if q.Unpack(b) != nil {
			t.Error("invalid DNS wire request")
			return
		}
		a := new(dns.Msg)
		a.SetReply(q)
		a.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 5}, A: net.ParseIP("203.0.113.8")}}
		b, _ = a.Pack()
		w.Header().Set("Content-Type", "application/dns-message")
		w.Write(b)
	}))
	defer doh.Close()
	p, count := testProxy(t)
	c := DefaultConfig()
	c.ProxyURL = p.URL
	c.DNSURL = doh.URL
	e, _ := NewEgress(c)
	e.client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: doh.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs}
	q := new(dns.Msg)
	q.SetQuestion("cage-test.invalid.", dns.TypeA)
	r, err := e.ExchangeDNS(context.Background(), q)
	if err != nil || len(r.Answer) != 1 {
		t.Fatalf("DNS: %v %v", r, err)
	}
	if count.Load() != 1 || queries.Load() != 1 {
		t.Fatal("DNS did not traverse proxy")
	}
	e.client.CloseIdleConnections()
	p.Close()
	if _, err = e.ExchangeDNS(context.Background(), q); err == nil {
		t.Fatal("DNS outage must fail")
	}
	if queries.Load() != 1 {
		t.Fatal("DNS fallback escaped")
	}
}

func TestDNSRetriesOnlyTransientFailuresThroughSameProxy(t *testing.T) {
	for _, status := range []int{503, 429, 400} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var requests atomic.Int32
			doh := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if requests.Add(1) <= 2 {
					w.WriteHeader(status)
					return
				}
				body, _ := io.ReadAll(r.Body)
				q := new(dns.Msg)
				if err := q.Unpack(body); err != nil {
					t.Error(err)
					return
				}
				answer := new(dns.Msg).SetReply(q)
				body, _ = answer.Pack()
				w.Write(body)
			}))
			defer doh.Close()
			p, connects := testProxy(t)
			defer p.Close()
			c := DefaultConfig()
			c.ProxyURL, c.DNSURL = p.URL, doh.URL
			e, err := NewEgress(c)
			if err != nil {
				t.Fatal(err)
			}
			defer e.client.CloseIdleConnections()
			e.client.Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: doh.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs}
			_, err = e.ExchangeDNS(t.Context(), new(dns.Msg).SetQuestion("example.com.", dns.TypeA))
			if status == 400 {
				if err == nil || requests.Load() != 1 {
					t.Fatalf("permanent failure retried: %v, %d", err, requests.Load())
				}
			} else if err != nil || requests.Load() != 3 {
				t.Fatalf("transient failure not recovered: %v, %d", err, requests.Load())
			}
			if connects.Load() == 0 {
				t.Fatal("DNS bypassed proxy")
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			before := requests.Load()
			if _, err := e.ExchangeDNS(ctx, new(dns.Msg).SetQuestion("example.com.", dns.TypeA)); err == nil {
				t.Fatal("cancel ignored")
			}
			if requests.Load() != before {
				t.Fatal("cancelled lookup sent request")
			}
		})
	}
}

func TestRejectHostLANAndUDP(t *testing.T) {
	e, _ := NewEgress(DefaultConfig())
	for _, addr := range []string{"127.0.0.1:80", "10.0.0.1:80", "192.168.1.1:80", "169.254.169.254:80", "[::1]:80", "[::ffff:127.0.0.1]:80", "100.64.0.1:80"} {
		if e.Allows("tcp", netip.MustParseAddrPort(addr)) {
			t.Errorf("host/LAN allowed: %s", addr)
		}
	}
	if e.Allows("udp", netip.MustParseAddrPort("1.1.1.1:53")) {
		t.Fatal("UDP allowed")
	}
	if !e.Allows("tcp", netip.MustParseAddrPort("1.1.1.1:443")) {
		t.Fatal("public TCP refused")
	}
}

func TestCredentialsDoNotAppearInErrors(t *testing.T) {
	p := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "bad", 407) }))
	defer p.Close()
	c := DefaultConfig()
	c.ProxyURL = strings.Replace(p.URL, "http://", "http://alice:secret@", 1)
	e, _ := NewEgress(c)
	_, err := e.DialContext(context.Background(), "tcp", "example.com:443")
	if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "alice") {
		t.Fatal("credential error handling")
	}
}

func TestProxyHostnameCannotUseHostResolver(t *testing.T) {
	c := DefaultConfig()
	c.ProxyURL = "socks5://proxy.example:1080"
	if c.Validate() == nil {
		t.Fatal("proxy hostname must not trigger bootstrap DNS")
	}
}
