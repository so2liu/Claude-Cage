// Package hostproxy routes host-side image and application requests through the host's
// proxy settings. Guest traffic must continue to use cage.Egress instead.
package hostproxy

import (
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/http/httpproxy"
)

var environment struct {
	sync.Mutex
	config *httpproxy.Config
}

// CaptureEnvironment preserves host proxy settings before the CLI removes them
// from child/guest environments. It never restores them to the process env.
func CaptureEnvironment() {
	environment.Lock()
	defer environment.Unlock()
	if environment.config == nil {
		environment.config = readEnvironment()
	}
}

func readEnvironment() *httpproxy.Config {
	cfg := httpproxy.FromEnvironment()
	all := os.Getenv("ALL_PROXY")
	if all == "" {
		all = os.Getenv("all_proxy")
	}
	if cfg.HTTPProxy == "" {
		cfg.HTTPProxy = all
	}
	if cfg.HTTPSProxy == "" {
		cfg.HTTPSProxy = all
	}
	return cfg
}

func NewTransport() *http.Transport {
	environment.Lock()
	cfg := environment.config
	if cfg == nil {
		cfg = readEnvironment()
	}
	copy := *cfg
	environment.Unlock()
	return transport(&copy, systemProxy)
}

// DownloadEnvironment restores only the captured proxy variables for a host
// downloader subprocess. Callers must not pass these into guest operations.
func DownloadEnvironment() []string {
	environment.Lock()
	defer environment.Unlock()
	cfg := environment.config
	if cfg == nil {
		cfg = readEnvironment()
	}
	return []string{
		"HTTP_PROXY=" + cfg.HTTPProxy, "http_proxy=" + cfg.HTTPProxy,
		"HTTPS_PROXY=" + cfg.HTTPSProxy, "https_proxy=" + cfg.HTTPSProxy,
		"NO_PROXY=" + cfg.NoProxy, "no_proxy=" + cfg.NoProxy,
		"ALL_PROXY=", "all_proxy=",
	}
}

func transport(env *httpproxy.Config, system func(*http.Request) (*url.URL, error)) *http.Transport {
	envProxy := env.ProxyFunc()
	return &http.Transport{
		Proxy: func(req *http.Request) (*url.URL, error) {
			value := env.HTTPProxy
			if req.URL.Scheme == "https" {
				value = env.HTTPSProxy
			}
			if value != "" {
				if _, err := proxyURL(value); err != nil {
					return nil, err
				}
				return envProxy(req.URL)
			}
			return system(req)
		},
		// No interface binding: host TUN/routing rules apply to these sockets.
		DialContext:       (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2: true, TLSHandshakeTimeout: 15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second, IdleConnTimeout: 90 * time.Second,
	}
}

func proxyURL(value string) (*url.URL, error) {
	value = strings.TrimSpace(value)
	if !strings.Contains(value, "://") {
		value = "http://" + value
	}
	u, err := url.Parse(value)
	if err != nil || u.Hostname() == "" {
		return nil, errors.New("主机代理地址无效，请检查系统代理设置")
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
		return u, nil
	default:
		return nil, errors.New("主机代理协议不受支持，请使用 HTTP 或 SOCKS5 系统代理")
	}
}

func staticProxy(req *http.Request, httpProxy, httpsProxy, bypass string) (*url.URL, error) {
	selected := httpProxy
	if req.URL.Scheme == "https" {
		selected = httpsProxy
	}
	if selected != "" {
		if _, err := proxyURL(selected); err != nil {
			return nil, err
		}
	}
	cfg := httpproxy.Config{HTTPProxy: httpProxy, HTTPSProxy: httpsProxy, NoProxy: strings.ReplaceAll(bypass, ";", ",")}
	u, err := cfg.ProxyFunc()(req.URL)
	if err != nil {
		return nil, errors.New("主机代理设置无效，请检查系统代理设置")
	}
	if u != nil {
		return proxyURL(u.String())
	}
	return nil, nil
}
