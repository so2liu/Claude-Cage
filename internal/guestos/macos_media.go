package guestos

import (
	"context"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/managedproxy"
)

// PrepareMacOSMedia downloads tools on the host through the selected Cage proxy.
// The CLI deliberately removes inherited host proxy settings before VM work;
// supply this environment only to the downloader, never to the guest or SSH.
func PrepareMacOSMedia(ctx context.Context, profile, kit, cache string, refresh bool, stdout, stderr io.Writer) error {
	cfg, err := cage.Load(filepath.Join(profile, "config.json"))
	if err != nil {
		return err
	}
	store, err := managedproxy.Applied(profile, cfg.ProxyURL, cfg.ProxyRevision)
	if err != nil {
		return err
	}
	// Installation can resume after a controller restart, when the selected
	// node is persisted but its core is stopped. This grants no guest access.
	if err := managedproxy.Start(ctx, profile, store); err != nil {
		return err
	}
	return prepareMacOSMedia(ctx, kit, cache, cfg.ProxyURL, refresh, stdout, stderr)
}

func prepareMacOSMedia(ctx context.Context, kit, cache, proxy string, refresh bool, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, "/bin/bash", filepath.Join(kit, "prepare-macos-media.sh"), cache)
	cmd.Env = macOSMediaEnvironment(os.Environ(), proxy, refresh)
	configureMediaCommand(cmd)
	cmd.WaitDelay = 5 * time.Second
	cmd.Stdout, cmd.Stderr = stdout, stderr
	return cmd.Run()
}

func macOSMediaEnvironment(parent []string, proxy string, refresh bool) []string {
	// curl's socks5 scheme resolves target names locally; socks5h delegates DNS
	// to the proxy, matching cage.Egress and avoiding host DNS leakage.
	u, _ := url.Parse(proxy) // cage.Load has already validated this URL.
	if u.Scheme == "socks5" {
		u.Scheme = "socks5h"
	}
	env := make([]string, 0, len(parent)+7)
	for _, entry := range parent {
		key, _, _ := strings.Cut(entry, "=")
		switch strings.ToLower(key) {
		case "http_proxy", "https_proxy", "all_proxy", "no_proxy", "ftp_proxy", "cage_refresh_tools":
			continue
		}
		env = append(env, entry)
	}
	for _, key := range []string{"http_proxy", "https_proxy", "all_proxy", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY"} {
		env = append(env, key+"="+u.String())
	}
	if refresh {
		env = append(env, "CAGE_REFRESH_TOOLS=1")
	}
	return env
}
