package cage

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/miekg/dns"
	"github.com/sylvinus/agent-vm/internal/netguard"
	"github.com/sylvinus/agent-vm/internal/usererror"
	"golang.org/x/net/proxy"
)

// Egress has one upstream and no direct fallback. It lives in the hostagent,
// outside the VM. The guest never receives the upstream credentials.
type Egress struct {
	upstream *url.URL
	dnsURL   string
	client   *http.Client
	public   netguard.Policy
	connect  proxyConnector
}

type proxyConnector interface {
	Dial(string, string) (net.Conn, error)
	DialContext(context.Context, string, string) (net.Conn, error)
}

func NewEgress(c Config) (*Egress, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	u, _ := url.Parse(c.ProxyURL)
	e := &Egress{upstream: u, dnsURL: c.DNSURL, connect: &net.Dialer{Timeout: 20 * time.Second, KeepAlive: 30 * time.Second}}
	e.client = &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{Proxy: nil, DialContext: e.DialContext, ForceAttemptHTTP2: true}, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("DNS redirect refused") }}
	return e, nil
}

func (e *Egress) Allows(network string, dst netip.AddrPort) bool {
	// UDP and ICMP have no fallback. Guest DNS uses the dedicated DoH path.
	return network == "tcp" && dst.Addr().Unmap().Is4() && e.public.Allows(network, dst)
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

func proxyConnectionError(ctx context.Context, action string, err error) error {
	// Closing a stalled handshake on cancellation can report net.ErrClosed.
	// Preserve the operation deadline so exit monitoring can classify a timeout.
	if ctx.Err() != nil {
		err = ctx.Err()
	} else if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		// The socket deadline can fire before the context timer publishes Err.
		var timeout net.Error
		if errors.As(err, &timeout) && timeout.Timeout() {
			err = errors.Join(context.DeadlineExceeded, err)
		}
	}
	return usererror.Wrap(action, err, "请检查代理；请求不会改为直连。")
}

// DialContext is used for all guest TCP connections and DoH. DNS names are
// passed to the proxy without resolving them on the host.
func (e *Egress) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, errors.New("cage permits TCP only")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if strings.HasPrefix(e.upstream.Scheme, "socks5") {
		var auth *proxy.Auth
		if e.upstream.User != nil {
			p, _ := e.upstream.User.Password()
			auth = &proxy.Auth{User: e.upstream.User.Username(), Password: p}
		}
		d, err := proxy.SOCKS5("tcp", e.upstream.Host, auth, e.connect)
		if err != nil {
			return nil, errors.New("invalid SOCKS5 configuration")
		}
		c, err := d.(proxy.ContextDialer).DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, proxyConnectionError(ctx, "连接指定 SOCKS5 代理", err)
		}
		return c, nil
	}
	conn, err := e.connect.DialContext(ctx, "tcp", e.upstream.Host)
	if err != nil {
		return nil, proxyConnectionError(ctx, "连接指定代理", err)
	}
	// Cancellation must interrupt a stalled CONNECT response, not only the
	// initial TCP connect. Stop the callback before returning a live tunnel.
	upstreamConn := conn
	stopCancel := context.AfterFunc(ctx, func() { _ = upstreamConn.Close() })
	defer stopCancel()
	ok := false
	defer func() {
		if !ok {
			conn.Close()
		}
	}()
	if deadline, yes := ctx.Deadline(); yes {
		conn.SetDeadline(deadline)
	}
	if e.upstream.Scheme == "https" {
		t := tls.Client(conn, &tls.Config{ServerName: e.upstream.Hostname(), MinVersion: tls.VersionTLS12})
		if err = t.HandshakeContext(ctx); err != nil {
			return nil, proxyConnectionError(ctx, "验证 HTTPS 代理的加密连接", err)
		}
		conn = t
	}
	req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: addr}, Host: addr, Header: make(http.Header)}
	if e.upstream.User != nil {
		p, _ := e.upstream.User.Password()
		req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(e.upstream.User.Username()+":"+p)))
	}
	if err = req.Write(conn); err != nil {
		return nil, proxyConnectionError(ctx, "发送代理 CONNECT 请求", err)
	}
	r := bufio.NewReader(conn)
	resp, err := http.ReadResponse(r, req)
	if err != nil {
		return nil, proxyConnectionError(ctx, "读取代理 CONNECT 响应", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("代理拒绝 CONNECT 请求（HTTP %d）；请求不会改为直连", resp.StatusCode)
	}
	conn.SetDeadline(time.Time{})
	ok = true
	return &bufferedConn{Conn: conn, reader: r}, nil
}

// ExchangeDNS forwards DNS wire messages over HTTPS via the same upstream.
// It never calls the host resolver, even when the upstream fails.
func (e *Egress) ExchangeDNS(ctx context.Context, q *dns.Msg) (*dns.Msg, error) {
	for _, question := range q.Question {
		// The initial exit policy monitors IPv4. Do not offer an unmeasured
		// guest IPv6 route; literal IPv6 destinations are denied by Allows too.
		if question.Qtype == dns.TypeAAAA {
			r := new(dns.Msg)
			r.SetReply(q)
			return r, nil
		}
		n := strings.ToLower(strings.TrimSuffix(question.Name, "."))
		if n == "localhost" || strings.HasSuffix(n, ".localhost") || strings.HasSuffix(n, ".internal") || strings.HasSuffix(n, ".local") {
			r := new(dns.Msg)
			r.SetRcode(q, dns.RcodeNameError)
			return r, nil
		}
	}
	wire, err := q.Pack()
	if err != nil {
		return nil, err
	}
	// Keep the original total deadline, but allow a fresh connection after an
	// intermittent EOF or overloaded DoH server. Every attempt uses this same
	// proxy-only transport and the configured endpoint.
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	for attempt := 0; attempt < 3; attempt++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		attemptCtx, done := context.WithTimeout(ctx, 6*time.Second)
		var answer *dns.Msg
		var retry bool
		answer, retry, err = e.exchangeDNSOnce(attemptCtx, q, wire)
		done()
		if err == nil || !retry {
			return answer, err
		}
		if attempt < 2 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt+1) * 150 * time.Millisecond):
			}
		}
	}
	return nil, err
}

func (e *Egress) exchangeDNSOnce(ctx context.Context, q *dns.Msg, wire []byte) (*dns.Msg, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.dnsURL, bytes.NewReader(wire))
	if err != nil {
		return nil, false, errors.New("invalid DNS request")
	}
	req.Header.Set("Content-Type", "application/dns-message")
	req.Header.Set("Accept", "application/dns-message")
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, true, errors.New("cage DNS upstream unavailable; check the configured DoH endpoint through the selected proxy")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, resp.StatusCode == 429 || resp.StatusCode >= 500, fmt.Errorf("cage DNS upstream returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 65536))
	if err != nil {
		return nil, true, errors.New("incomplete DNS upstream response")
	}
	r := new(dns.Msg)
	if err = r.Unpack(body); err != nil {
		return nil, false, errors.New("invalid DNS upstream response")
	}
	if !r.Response || r.Id != q.Id {
		return nil, false, errors.New("mismatched DNS upstream response")
	}
	return r, false, nil
}
