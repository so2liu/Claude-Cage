package cage

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestExitProbeClassifiesProxyTimeout(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	var connections atomic.Int32
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			connections.Add(1)
			go func() { defer conn.Close(); _, _ = io.Copy(io.Discard, conn) }()
		}
	}()
	cfg := DefaultConfig()
	cfg.ProxyURL = "http://" + listener.Addr().String()
	ctx, cancel := context.WithTimeout(context.Background(), 14*time.Second)
	defer cancel()
	ip, err := ProbeExitIP(ctx, cfg)
	if ip != "" || !errors.Is(err, context.DeadlineExceeded) || connections.Load() != 2 {
		t.Fatalf("two timed-out proxy probes = %q, %v, connections=%d", ip, err, connections.Load())
	}
}

// The child uses a private test CA, so the real ProbeExitIP entry point verifies
// TLS without changing production trust or replacing the HTTP transport.
func TestExitProbeChild(t *testing.T) {
	proxy := os.Getenv("CAGE_EXIT_PROBE_TEST_PROXY")
	if proxy == "" {
		t.Skip("subprocess fixture")
	}
	certificate, err := os.ReadFile(os.Getenv("CAGE_EXIT_PROBE_TEST_CA"))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certificate) {
		t.Fatal("invalid test CA")
	}
	x509.SetFallbackRoots(roots)
	cfg := DefaultConfig()
	cfg.ProxyURL = proxy
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	want := os.Getenv("CAGE_EXIT_PROBE_TEST_IP")
	ip, err := ProbeExitIP(ctx, cfg)
	if want != "" {
		if err != nil || ip != want {
			t.Fatalf("host probe = %q, %v; want %q", ip, err, want)
		}
	} else if err == nil || ip != "" || strings.Contains(err.Error(), "private-value") || strings.Contains(err.Error(), "test-password") {
		t.Fatalf("failed host probe returned an address or leaked a response: %q, %v", ip, err)
	}
	gate, err := NewEgressGate(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Close("test complete")
	if err := gate.BeginCheck(); err != nil {
		t.Fatal(err)
	}
	connection, err := gate.DialContext(ctx, "tcp", DiagnosticAddress)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
	_, err = io.WriteString(connection, "GET /egress HTTP/1.1\r\nHost: 198.18.0.254\r\nCookie: private-value\r\nAuthorization: private-value\r\n\r\n")
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(connection), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var result struct{ IP, Error string }
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if want != "" {
		if response.StatusCode != 200 || result.IP != want {
			t.Fatalf("guest diagnostic lost IPv4 JSON contract: %d %+v", response.StatusCode, result)
		}
	} else if response.StatusCode != 503 || result.IP != "" || result.Error != "proxy_unavailable" {
		t.Fatalf("failed guest diagnostic did not retain its fixed error contract: %d %+v", response.StatusCode, result)
	}
}

func TestExitProbeUsesIPv4ServiceThroughProxy(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"ipv4.icanhazip.com", "api.ipify.org"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	certificate, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate}), 0600); err != nil {
		t.Fatal(err)
	}
	for _, scheme := range []string{"http", "socks5"} {
		for _, tc := range []struct {
			name, body, want string
			status           int
		}{
			{"public-ipv4", "8.8.8.8\n", "8.8.8.8", 200},
			{"fallback-after-unavailable", "8.8.8.8\n", "8.8.8.8", 503},
			{"fallback-after-invalid", "8.8.8.8\n", "8.8.8.8", 200},
			{"whitespace", " \r\n1.1.1.1\r\n", "1.1.1.1", 200},
			{"ipv6", "2606:4700:4700::1111\n", "", 200},
			{"mapped-ipv4", "::ffff:8.8.8.8", "", 200},
			{"private-ipv4", "192.168.1.1", "", 200},
			{"loopback", "127.0.0.1", "", 200},
			{"link-local", "169.254.1.1", "", 200},
			{"multicast", "224.0.0.1", "", 200},
			{"empty", "", "", 200},
			{"json-response", `{"ip":"8.8.8.8","secret":"private-value"}`, "", 200},
			{"multiple-addresses", "8.8.8.8\n1.1.1.1", "", 200},
			{"oversized", "8.8.8.8" + strings.Repeat(" ", 4096), "", 200},
			{"short-read", "8.8.8.8", "", 200},
			{"unavailable", "private-value", "", 503},
			{"redirect", "private-value", "", 302},
		} {
			t.Run(scheme+"/"+tc.name, func(t *testing.T) {
				var requests atomic.Int32
				upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
					requests.Add(1)
					if (request.Host != "ipv4.icanhazip.com" && request.Host != "api.ipify.org") || request.URL.RequestURI() != "/" || request.Method != "GET" || request.Header.Get("User-Agent") != "Cage-network-check/1" || request.Header.Get("Cookie") != "" || request.Header.Get("Authorization") != "" || request.Header.Get("Proxy-Authorization") != "" {
						t.Error("probe request changed destination or forwarded guest/proxy data")
					}
					if request.Host == "api.ipify.org" {
						if strings.HasPrefix(tc.name, "fallback-") {
							_, _ = io.WriteString(w, tc.body)
							return
						}
						_, _ = io.WriteString(w, `{"ip":"2606:4700:4700::1111"}`)
						return
					}
					if tc.name == "fallback-after-invalid" {
						_, _ = io.WriteString(w, "192.168.1.1")
						return
					}
					w.Header().Set("Content-Type", "text/plain")
					if tc.name == "redirect" {
						w.Header().Set("Location", "https://private-value.invalid/")
					}
					if tc.name == "short-read" {
						w.Header().Set("Content-Length", "50")
					}
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, tc.body)
				}))
				upstream.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{certificate}, PrivateKey: key}}}
				upstream.StartTLS()
				defer upstream.Close()
				proxy, connects := exitProbeProxy(t, scheme, upstream.Listener.Addr().String())
				command := exec.Command(os.Args[0], "-test.run=^TestExitProbeChild$", "-test.timeout=15s")
				command.Env = append(os.Environ(), "GODEBUG=x509usefallbackroots=1", "CAGE_EXIT_PROBE_TEST_PROXY="+proxy, "CAGE_EXIT_PROBE_TEST_CA="+caFile, "CAGE_EXIT_PROBE_TEST_IP="+tc.want)
				if output, err := command.CombinedOutput(); err != nil {
					t.Fatalf("real probe entry failed: %v\n%s", err, output)
				}
				wantRequests := int32(2)
				if tc.want == "" || strings.HasPrefix(tc.name, "fallback-") {
					wantRequests = 4
				}
				if connects.Load() != wantRequests || requests.Load() != wantRequests {
					t.Fatalf("host and guest probes must use only the configured proxy: connects=%d requests=%d", connects.Load(), requests.Load())
				}
			})
		}
	}
}

func TestExitProbeProxyRejectionDoesNotFallbackOrLeakResponse(t *testing.T) {
	var requests atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "private-proxy-response")
	}))
	defer proxy.Close()
	cfg := DefaultConfig()
	cfg.ProxyURL = proxy.URL
	ip, err := ProbeExitIP(context.Background(), cfg)
	if err == nil || ip != "" || requests.Load() != 2 || strings.Contains(err.Error(), "private-proxy-response") {
		t.Fatalf("proxy rejection escaped the configured route or leaked its response: IP=%q error=%v requests=%d", ip, err, requests.Load())
	}
}

func TestExitProbePublicIPv4Service(t *testing.T) {
	if os.Getenv("CAGE_EXIT_PROBE_LIVE") != "1" {
		t.Skip("requires the public IPv4 service through an isolated local proxy")
	}
	for _, scheme := range []string{"http", "socks5"} {
		t.Run(scheme, func(t *testing.T) {
			proxy, requests := exitProbeProxy(t, scheme, "ipv4.icanhazip.com:443")
			cfg := DefaultConfig()
			cfg.ProxyURL = proxy
			ip, err := ProbeExitIP(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			address, err := netip.ParseAddr(ip)
			if err != nil || !address.Is4() || !address.IsGlobalUnicast() || address.IsPrivate() || requests.Load() != 1 {
				t.Fatal("public service did not return IPv4 through the selected proxy")
			}
			t.Log("public endpoint returned a public IPv4 through the isolated proxy")
		})
	}
}

// The local fixture accepts CONNECT or SOCKS domain requests and maps the fixed
// public hostname to an isolated TLS server without using the application's dialer.
func exitProbeProxy(t *testing.T, scheme, upstream string) (string, *atomic.Int32) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	count := new(atomic.Int32)
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer connection.Close()
				_ = connection.SetDeadline(time.Now().Add(10 * time.Second))
				reader := bufio.NewReader(connection)
				var target string
				if scheme == "http" {
					request, err := http.ReadRequest(reader)
					if err != nil || request.Method != "CONNECT" {
						t.Error("expected HTTP CONNECT")
						return
					}
					if request.Header.Get("Proxy-Authorization") != "Basic dGVzdC11c2VyOnRlc3QtcGFzc3dvcmQ=" {
						t.Error("HTTP proxy authentication was lost")
						return
					}
					target = request.Host
					_, _ = io.WriteString(connection, "HTTP/1.1 200 Connection Established\r\n\r\n")
				} else {
					var header [4]byte
					if _, err := io.ReadFull(reader, header[:2]); err != nil {
						return
					}
					if _, err := io.CopyN(io.Discard, reader, int64(header[1])); err != nil {
						return
					}
					_, _ = connection.Write([]byte{5, 2})
					if _, err := io.ReadFull(reader, header[:2]); err != nil {
						return
					}
					username := make([]byte, int(header[1]))
					if _, err := io.ReadFull(reader, username); err != nil {
						return
					}
					passwordLength, err := reader.ReadByte()
					if err != nil {
						return
					}
					password := make([]byte, int(passwordLength))
					if _, err := io.ReadFull(reader, password); err != nil {
						return
					}
					if string(username) != "test-user" || string(password) != "test-password" {
						t.Error("SOCKS proxy authentication was lost")
						return
					}
					_, _ = connection.Write([]byte{1, 0})
					if _, err := io.ReadFull(reader, header[:]); err != nil {
						return
					}
					if header[0] != 5 || header[1] != 1 || header[3] != 3 {
						t.Error("SOCKS probe resolved its hostname outside the proxy")
						return
					}
					length, err := reader.ReadByte()
					if err != nil {
						return
					}
					host := make([]byte, length)
					if _, err := io.ReadFull(reader, host); err != nil {
						return
					}
					if _, err := io.ReadFull(reader, header[:2]); err != nil {
						return
					}
					target = net.JoinHostPort(string(host), fmt.Sprint(binary.BigEndian.Uint16(header[:2])))
					_, _ = connection.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 80})
				}
				if target != "ipv4.icanhazip.com:443" && target != "api.ipify.org:443" {
					t.Error("probe followed a redirect or changed destination")
					return
				}
				count.Add(1)
				remote, err := net.DialTimeout("tcp", upstream, time.Second)
				if err != nil {
					t.Error(err)
					return
				}
				defer remote.Close()
				go func() { _, _ = io.Copy(remote, reader); _ = remote.Close() }()
				_, _ = io.Copy(connection, remote)
			}()
		}
	}()
	return scheme + "://test-user:test-password@" + listener.Addr().String(), count
}
