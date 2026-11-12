package workbench

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/sylvinus/agent-vm/internal/cage"
)

func boolp(v bool) *bool { return &v }

func TestGuestProxyEndpointUsesGatewayForHostProxies(t *testing.T) {
	cfg := cage.DefaultConfig()
	for _, tc := range []struct {
		proxy, gateway, host string
		port                 int
	}{
		{"socks5://127.0.0.1:17896", "192.168.5.2", "192.168.5.2", 17896},
		{"http://0.0.0.0:7890", "10.88.0.1", "10.88.0.1", 7890},
		{"http://[::1]:7890", "192.168.5.2", "192.168.5.2", 7890},
		{"http://proxy.example.com:8080", "192.168.5.2", "proxy.example.com", 8080},
		{"http://localhost:7890", "192.168.5.2", "192.168.5.2", 7890},
		{"http://[2001:db8::1]:8080", "192.168.5.2", "2001:db8::1", 8080},
		{"http://203.0.113.9:8080", "192.168.5.2", "203.0.113.9", 8080},
		{"http://192.168.1.20:7890", "192.168.5.2", "192.168.1.20", 7890},
		{"http://127.0.0.1", "192.168.5.2", "", 0},
	} {
		cfg.ProxyURL = tc.proxy
		host, port := guestProxyEndpoint(cfg, tc.gateway)
		if host != tc.host || port != tc.port {
			t.Errorf("%s via %s: got %s:%d, want %s:%d", tc.proxy, tc.gateway, host, port, tc.host, tc.port)
		}
	}
}

// Exercise the embedded boundary entry and decoder with denied sockets and a
// fixed Linux route/resolver fixture. No host network or sudo command is used.
func TestSafetyProbeBoundaryReportsLinuxEvidence(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Linux probe requires POSIX file flags")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	dir := t.TempDir()
	resolver := filepath.Join(dir, "resolv.conf")
	if err := os.WriteFile(resolver, []byte("nameserver 127.0.0.53\nnameserver 192.168.5.2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(guestSafetyProbe, "/etc/resolv.conf", filepath.ToSlash(resolver))
	script = strings.ReplaceAll(script, "/run/systemd/resolve/resolv.conf", filepath.ToSlash(filepath.Join(dir, "absent")))
	prelude := `import socket, subprocess
class DeniedSocket:
    def __init__(self, family, kind): self.family = family
    def settimeout(self, timeout): pass
    def connect(self, address):
        assert (":" in address[0]) == (self.family == socket.AF_INET6)
        raise ConnectionRefusedError()
    def sendto(self, payload, address): return len(payload)
    def close(self): pass
def no_dns(*args, **kwargs): raise socket.gaierror()
def linux_command(command, **kwargs):
    if command[0] == "ip": return subprocess.CompletedProcess(command, 0, "[]", "")
    raise OSError("no subprocess allowed")
socket.socket = DeniedSocket
socket.getaddrinfo = no_dns
subprocess.run = linux_command
`
	input := boundaryProbeInput(17779, "test", filepath.Join(dir, "private"), filepath.Join(dir, "shared"), "challenge")
	input["proxy_host"], input["proxy_port"] = "2001:db8::1", 7890
	data, _ := json.Marshal(input)
	cmd := exec.Command(python, "-I", "-c", prelude+script, base64.StdEncoding.EncodeToString(data))
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("probe failed: %v %s", err, output)
	}
	var v guestObservation
	if err := json.Unmarshal(output, &v); err != nil {
		t.Fatal(err)
	}
	if issue, _ := hostReachIssue(v, true, true); issue != "" {
		t.Fatalf("host evidence: %s: %s", issue, output)
	}
	if issue, _ := dnsBypassIssue(v, true); issue != "" {
		t.Fatalf("DNS evidence: %s: %s", issue, output)
	}
	if v.IPv6GlobalAddresses == nil || *v.IPv6GlobalAddresses != 0 {
		t.Fatalf("missing address observation: %s", output)
	}
}

func TestHostReachAndDNSBypassIssues(t *testing.T) {
	var v guestObservation
	if issue, _ := hostReachIssue(v, false, false); issue != "" {
		t.Fatalf("empty observation reported host reach: %s", issue)
	}
	// Windows and macOS probes do not report the Linux-only fields; they add no demands there.
	if issue, _ := dnsBypassIssue(v, false); issue != "" {
		t.Fatalf("non-Linux observation demanded Linux evidence: %s", issue)
	}
	if issue, confirmed := dnsBypassIssue(v, true); issue == "" || confirmed {
		t.Fatalf("missing Linux DNS evidence must be unknown, not passed or failed: %q %t", issue, confirmed)
	}
	v.DNSUDPAttempted = true
	v.DNSTCPConnected, v.DoTConnected = boolp(false), boolp(false)
	v.Resolvers = resolverObservation{Count: intp(2), Confined: boolp(true)}
	if issue, _ := dnsBypassIssue(v, true); issue != "" {
		t.Fatalf("confined resolvers rejected: %s", issue)
	}
	v.Resolvers.Confined = boolp(false)
	if issue, confirmed := dnsBypassIssue(v, true); !confirmed || !strings.Contains(issue, "解析器") {
		t.Fatalf("resolver outside the environment not confirmed: %q %t", issue, confirmed)
	}
	v.Resolvers = resolverObservation{Count: intp(-1)}
	if issue, confirmed := dnsBypassIssue(v, true); issue == "" || confirmed {
		t.Fatalf("unreadable resolver configuration must stay unknown: %q %t", issue, confirmed)
	}
	for name, mutate := range map[string]func(*guestObservation){
		"tcp/53":  func(v *guestObservation) { v.DNSTCPConnected = boolp(true) },
		"dot/853": func(v *guestObservation) { v.DoTConnected = boolp(true) },
	} {
		w := guestObservation{DNSUDPAttempted: true, Resolvers: resolverObservation{Count: intp(1), Confined: boolp(true)}}
		mutate(&w)
		for _, linux := range []bool{true, false} {
			if issue, confirmed := dnsBypassIssue(w, linux); !confirmed || issue == "" {
				t.Errorf("%s reached (linux=%t) not confirmed: %q", name, linux, issue)
			}
		}
	}
	if issue, _ := hostReachIssue(guestObservation{LinkLocalConnected: boolp(true)}, true, false); !strings.Contains(issue, "169.254.169.254") {
		t.Fatalf("link-local reach not reported: %q", issue)
	}
	if issue, _ := hostReachIssue(guestObservation{ProxyEndpointConnected: boolp(true)}, true, true); !strings.Contains(issue, "代理端口") {
		t.Fatalf("proxy endpoint reach not reported: %q", issue)
	}
}

// The Linux probe's boundary output decodes into the observation the checks read.
func TestLinuxProbeObservationDecodes(t *testing.T) {
	data := `{"tz":"Etc/UTC","lang":"en_US.UTF-8","lc_all":"en_US.UTF-8","system_timezone":"Etc/UTC","locale_valid":true,
"public_connected":false,"management_connected":false,"link_local_connected":false,"proxy_endpoint_connected":false,
"ipv6_connected":false,"ipv6_default_routes":0,"ipv6_global_addresses":0,
"udp_attempted":true,"dns_udp_attempted":true,"dns_tcp_connected":false,"dot_connected":false,
"resolvers":{"count":2,"confined":true},"public_dns_answered":false,"private_file":"unreachable","shared_file":true}`
	var v guestObservation
	if err := json.Unmarshal([]byte(data), &v); err != nil {
		t.Fatal(err)
	}
	hostIssue, _ := hostReachIssue(v, true, true)
	if issue, _ := dnsBypassIssue(v, true); issue != "" || hostIssue != "" || v.Resolvers.Confined == nil || !*v.Resolvers.Confined || v.IPv6GlobalAddresses == nil || *v.IPv6GlobalAddresses != 0 {
		t.Fatalf("probe output misread: %+v (%s)", v, issue)
	}
}
