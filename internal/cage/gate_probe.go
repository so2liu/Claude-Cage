package cage

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
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
	"github.com/sylvinus/agent-vm/internal/diagnostics"
	"github.com/sylvinus/agent-vm/internal/usererror"
)

// ProbeExitIP sends a fixed request through the configured proxy. It does not
// include user headers, credentials, a report or arbitrary guest data.
func ProbeExitIP(ctx context.Context, cfg Config) (string, error) {
	e, err := NewEgress(cfg)
	if err != nil {
		return "", err
	}
	return probeExitIP(ctx, e)
}

func probeExitIP(ctx context.Context, e *Egress) (string, error) {
	transport := &http.Transport{Proxy: nil, DialContext: e.DialContext, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("probe redirect refused") }}
	// The proxy resolves this name remotely. An IPv4-only service measures the
	// same address family as the guest policy even when the proxy is dual-stack.
	allTimedOut := true
	var failures []string
	for _, service := range []struct{ id, name, endpoint string }{
		{"icanhazip", "主检测服务", "https://ipv4.icanhazip.com/"},
		{"ipify", "备用检测服务", "https://api.ipify.org/"},
	} {
		started := time.Now()
		ip, err := queryExitIP(ctx, client, service.endpoint)
		if err == nil {
			return ip, nil
		}
		// Fixed service IDs survive URL redaction in exported diagnostics.
		diagnostics.Log("warn", "exit-probe", fmt.Sprintf("service=%s duration_ms=%d error=%v cause=%v", service.id, time.Since(started).Milliseconds(), err, errors.Unwrap(err)))
		failures = append(failures, service.name+"："+err.Error())
		var networkError net.Error
		if !errors.Is(err, context.DeadlineExceeded) && !(errors.As(err, &networkError) && networkError.Timeout()) {
			allTimedOut = false
		}
		if ctx.Err() != nil {
			break
		}
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return "", usererror.Wrap("通过所选代理查询出口 IP", ctx.Err(), "外网保持关闭。")
	}
	if allTimedOut {
		return "", usererror.New("通过所选代理查询出口 IP", strings.Join(failures, "；"), "出口检测超时，请稍后重试。", context.DeadlineExceeded)
	}
	return "", errors.New("通过所选代理查询出口 IP 失败：" + strings.Join(failures, "；") + "。未能确认出口时，外网保持关闭。")
}

func queryExitIP(ctx context.Context, client *http.Client, endpoint string) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	req.Header.Set("User-Agent", "Cage-network-check/1")
	resp, err := client.Do(req)
	if err != nil {
		var requestError *url.Error
		if errors.As(err, &requestError) {
			err = requestError.Err
		}
		return "", usererror.Display("通过所选代理查询出口 IP", err, "请检查网络与代理，再重新检测。")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("查询出口 IP 失败：检测服务返回 HTTP %d。请检查所选代理，稍后重新检测；外网保持关闭。", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4097))
	if err != nil {
		return "", usererror.Wrap("读取出口检测响应", err, "请稍后重新检测。")
	}
	if len(body) > 4096 {
		return "", errors.New("查询出口 IP 失败：检测服务没有返回有效的 IP 记录。请更换可用节点或稍后重新检测；外网保持关闭。")
	}
	ip, err := netip.ParseAddr(strings.TrimSpace(string(body)))
	if err != nil || !ip.Is4() || !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return "", errors.New("出口检测结果不是有效公网 IPv4 地址")
	}
	return ip.String(), nil
}

func probeDNS(ctx context.Context, e *Egress) error {
	q := new(dns.Msg)
	q.SetQuestion("example.com.", dns.TypeA)
	r, err := e.ExchangeDNS(ctx, q)
	if err != nil {
		return err
	}
	if r.Rcode != dns.RcodeSuccess {
		return errors.New("DNS probe did not resolve its fixed target")
	}
	for _, answer := range r.Answer {
		if a, ok := answer.(*dns.A); ok && a.A.IsGlobalUnicast() && !a.A.IsPrivate() {
			return nil
		}
	}
	return errors.New("DNS probe returned no usable answer")
}

func (g *EgressGate) dialDiagnostic(ctx context.Context) (net.Conn, error) {
	_, done, epoch, err := g.operation(ctx, true)
	if err != nil {
		return nil, err
	}
	defer done()
	client, server := net.Pipe()
	tracked, err := g.track(client, epoch)
	if err != nil {
		server.Close()
		return nil, err
	}
	g.mu.Lock()
	if !g.validLocked(epoch) {
		g.mu.Unlock()
		_ = tracked.Close()
		_ = server.Close()
		return nil, ErrGateClosed
	}
	parent, fault := g.epochContext, g.state.Fault
	g.mu.Unlock()
	go g.serveDiagnostic(parent, server, fault)
	return tracked, nil
}

// This synthetic HTTP service terminates guest input locally. It constructs
// fixed upstream probes instead of forwarding a CONNECT target, URL, query,
// body or headers. It is not a proxy to allowlisted shared CDN addresses.
func (g *EgressGate) serveDiagnostic(parent context.Context, conn net.Conn, fault bool) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	ctx, cancel := context.WithTimeout(parent, 14*time.Second)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	req, err := http.ReadRequest(bufio.NewReader(io.LimitReader(conn, 8192)))
	if err != nil {
		return
	}
	defer req.Body.Close()
	host := strings.TrimSuffix(strings.ToLower(req.Host), ":80")
	if req.Method != "GET" || host != DiagnosticHost || req.URL.IsAbs() || req.URL.RawQuery != "" || req.ContentLength > 0 || len(req.TransferEncoding) != 0 ||
		(req.URL.Path != "/" && req.URL.Path != "/egress" && req.URL.Path != "/dns") {
		writeDiagnostic(conn, 400, map[string]string{"error": "fixed_probe_only"})
		return
	}
	if req.URL.Path == "/" {
		writeDiagnostic(conn, 200, map[string]string{"service": "cage-diagnostic"})
		return
	}
	g.mu.Lock()
	if g.probeRequests >= 32 {
		g.mu.Unlock()
		writeDiagnostic(conn, 429, map[string]string{"error": "probe_limit"})
		return
	}
	g.probeRequests++
	g.mu.Unlock()
	e := g.diagnostic
	if fault {
		e = g.failedDiagnostic
	}
	if req.URL.Path == "/dns" {
		err = probeDNS(ctx, e)
		g.mu.Lock()
		g.state.Counters.ProbeDNS++
		if fault && err != nil {
			g.state.Counters.FaultDNS++
		}
		g.mu.Unlock()
		if err != nil {
			writeDiagnostic(conn, 503, map[string]string{"error": "dns_unavailable"})
			return
		}
		writeDiagnostic(conn, 200, map[string]bool{"ok": true})
		return
	}
	ip, err := probeExitIP(ctx, e)
	g.mu.Lock()
	g.state.Counters.ProbeTCP++
	if fault && err != nil {
		g.state.Counters.FaultTCP++
	}
	g.mu.Unlock()
	if err != nil {
		writeDiagnostic(conn, 503, map[string]string{"error": "proxy_unavailable"})
		return
	}
	writeDiagnostic(conn, 200, map[string]string{"ip": ip})
}

func writeDiagnostic(conn net.Conn, status int, value any) {
	body, _ := json.Marshal(value)
	resp := http.Response{StatusCode: status, Status: fmt.Sprintf("%d %s", status, http.StatusText(status)), ProtoMajor: 1, ProtoMinor: 1,
		Header: http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}, "Access-Control-Allow-Origin": {"*"}, "Connection": {"close"}},
		Body:   io.NopCloser(bytes.NewReader(body)), ContentLength: int64(len(body)), Close: true}
	_ = resp.Write(conn)
}
