package workbench

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"al.essio.dev/pkg/shellescape"
	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/guestos"
	"github.com/sylvinus/agent-vm/internal/managedproxy"
	"go.yaml.in/yaml/v3"
)

//go:embed macos_safety_probe.cjs
var macOSSafetyProbe string

func macOSZone(zone string) string {
	switch zone {
	case "+00:00", "GMT", "UTC", "Etc/GMT", "Etc/UTC":
		return "UTC"
	default:
		return zone
	}
}
func macOSLocale(raw string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSuffix(raw, ".UTF-8"), "_", "-"))
}

func (s *Server) guestSettingsOK(v guestObservation, cfg cage.Config) bool {
	if s.windowsGuest != nil {
		return windowsGuestSettingsMatch(v, cfg)
	}
	if s.macosGuest == nil {
		return guestSettingsMatch(v, cfg)
	}
	return macOSZone(v.TZ) == macOSZone(cfg.Timezone) && macOSZone(v.SystemTimezone) == macOSZone(cfg.Timezone) &&
		macOSLocale(v.ProcessLocale) == macOSLocale(cfg.Locale) && macOSLocale(v.SystemLocale) == macOSLocale(cfg.Locale) &&
		macOSLocale(v.SystemLanguage) == macOSLocale(cfg.Locale)
}

func (s *Server) browserSettingsIssue(v browserObservation, cfg cage.Config) string {
	if s.windowsGuest != nil {
		if !strings.HasPrefix(v.Platform, "Win") {
			return "客机浏览器没有报告 Windows 平台"
		}
		if effectiveWindowsZone(v.Timezone) != effectiveWindowsZone(cfg.Timezone) || effectiveWindowsZone(v.ExpectedTimezone) != effectiveWindowsZone(cfg.Timezone) {
			return "客机浏览器实际时区与配置不符"
		}
		if !strings.EqualFold(v.Language, cfg.BrowserLanguage) {
			return "客机浏览器实际语言与配置不符"
		}
		return ""
	}
	if s.macosGuest == nil {
		return browserSettingsMismatch(v, cfg)
	}
	if !strings.HasPrefix(v.Platform, "Mac") {
		return "客机浏览器没有报告 macOS 平台"
	}
	// Intl canonicalizes IANA aliases, including Etc/UTC -> UTC. The browser
	// must independently report the same effective zone as the applied config.
	if macOSZone(v.Timezone) != macOSZone(cfg.Timezone) || macOSZone(v.ExpectedTimezone) != macOSZone(cfg.Timezone) {
		return "客机浏览器实际时区与配置不符"
	}
	if !strings.EqualFold(v.Language, cfg.BrowserLanguage) {
		return "客机浏览器实际语言与配置不符"
	}
	return ""
}

func (s *Server) macOSGuestObservation(ctx context.Context, in map[string]any) (guestObservation, error) {
	var result guestObservation
	data, err := json.Marshal(in)
	if err != nil {
		return result, err
	}
	node := "/usr/local/libexec/cage-workbench-browser-4.145.0-pw1.65.0/editor/lib/node"
	out, err := s.guestControl(ctx, "exec "+shellescape.QuoteCommand([]string{node, "-e", macOSSafetyProbe, base64.StdEncoding.EncodeToString(data)}), "safety-probe")
	if err != nil || json.Unmarshal(out, &result) != nil {
		return result, errors.New("macOS 客机未返回完整的网络观测")
	}
	if in["stage"] == "boundary" {
		private, _ := in["private_file"].(string)
		result.PrivateFile = s.checkMacOSPrivateFile(ctx, private)
		shared, _ := in["shared_file"].(string)
		challenge, _ := in["challenge"].(string)
		result.SharedFile = s.checkMacOSSharedFile(ctx, shared, challenge)
	}
	return result, nil
}

// Use only the VM provisioning identity for this fixed read-only operation.
// The GUI user never receives an administrator key, password, or root shell.
func (s *Server) checkMacOSPrivateFile(ctx context.Context, private string) string {
	if filepath.Dir(private) != s.profile || !strings.HasPrefix(filepath.Base(private), ".cage-private-probe-") {
		return "unknown"
	}
	user, err := guestos.MacOSProvisionUser(s.profile)
	if err != nil {
		return "unknown"
	}
	base, err := guestos.MacOSSSHArgs(s.profile, user)
	if err != nil {
		return "unknown"
	}
	perl := `$> == 0 or exit 1; if (sysopen(my $fh,$ARGV[0],O_RDONLY|O_NONBLOCK)) {print "readable"} elsif ($! == ENOENT || $! == EACCES || $! == ENOTDIR) {print "unreachable"} else {print "unknown"}`
	op := shellescape.QuoteCommand([]string{"/usr/bin/sudo", "-S", "-p", "", "/usr/bin/env", "-i", "PATH=/usr/bin:/bin", "/usr/bin/perl", "-MFcntl=:DEFAULT", "-MErrno=ENOENT,EACCES,ENOTDIR", "-e", perl, "--", private})
	cmd := exec.CommandContext(ctx, sshExecutable(), append(base, "-T", s.sshTarget(), "--", "exec "+op+` < "$HOME/password"`)...)
	cmd.Env = sshEnvironment()
	out, err := cmd.Output()
	if err != nil {
		return "unknown"
	}
	result := strings.TrimSpace(string(out))
	if result != "unreachable" && result != "readable" {
		return "unknown"
	}
	return result
}

// The approved GUI login, rather than sshd, has the security-scoped bookmark.
// Open the real project's terminal and send only the bounded marker operation.
func (s *Server) checkMacOSSharedFile(ctx context.Context, shared, challenge string) bool {
	host := filepath.Dir(shared)
	name := filepath.Base(shared)
	if !strings.HasPrefix(name, ".cage-share-probe-") || challenge == "" {
		return false
	}
	s.mu.Lock()
	share := s.projectShares[host]
	approved := share != nil && share.detach != nil
	s.mu.Unlock()
	if !approved {
		return false
	}
	probe, err := os.Open(shared)
	if err != nil {
		return false
	}
	defer probe.Close()
	info, err := probe.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	dir := macOSProjectPath(s.macosGuest.User, host)
	op := `p=` + shellescape.Quote(name) + `; if test -f "$p" && test ! -L "$p" && test "$(cat "$p")" = ` + shellescape.Quote(challenge) + `; then printf %s ` + shellescape.Quote(challenge+":guest") + ` > "$p"; fi; exit`
	cmd := s.macOSSSHCommand(ctx, "-tt", s.sshTarget(), "--", "exec /usr/local/libexec/cage-macos-terminal "+shellescape.Quote(dir))
	tty, err := startTerminal(cmd, 80, 24)
	if err != nil {
		return false
	}
	defer func() { _ = tty.Close(); _ = tty.Kill(); _ = tty.Wait() }()
	// OpenSSH switches its outer terminal to raw mode during authentication.
	// Sending before it does so can discard typeahead. Wait for the mounted
	// project's own login shell to emit its first prompt through the broker.
	ready := make(chan bool, 1)
	go func() { var buf [4096]byte; n, err := tty.Read(buf[:]); ready <- n > 0 && err == nil }()
	select {
	case ok := <-ready:
		if !ok {
			return false
		}
	case <-ctx.Done():
		return false
	case <-time.After(5 * time.Second):
		return false
	}
	if _, err := tty.Write([]byte(op + "\r")); err != nil {
		return false
	}
	timer := time.NewTimer(6 * time.Second)
	defer timer.Stop()
	tick := time.NewTicker(80 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return false
		case <-tick.C:
			var buf [257]byte
			n, _ := probe.ReadAt(buf[:], 0)
			if bytes.Equal(buf[:n], []byte(challenge+":guest")) {
				return true
			}
		}
	}
}

func (s *Server) macOSVMLayout() (bool, bool) {
	data, err := os.ReadFile(filepath.Join(s.instanceDir(), "lima.yaml"))
	if err != nil {
		return false, false
	}
	var spec struct {
		OS               string `yaml:"os"`
		Driver           string `yaml:"vmType"`
		Mounts, Networks []any
		Extra            []any `yaml:"additionalDisks"`
	}
	if yaml.Unmarshal(data, &spec) != nil || spec.OS != "Darwin" || spec.Driver != "vz" {
		return false, false
	}
	return spec.Mounts != nil && len(spec.Mounts) == 0 && len(spec.Extra) == 0, spec.Networks != nil && len(spec.Networks) == 0
}

func (s *Server) macOSBrowserObservation(ctx context.Context, cfg cage.Config, network bool) (browserObservation, error) {
	var observation browserObservation
	data, err := s.nativeBrowserClient().request(ctx, "POST", "/audit", map[string]any{"timezone": cfg.Timezone, "network": network})
	if err != nil || json.Unmarshal(data, &observation) != nil || observation.Platform == "" {
		return observation, errors.New("无法在虚拟 Mac 的实际浏览器页面读取观测结果")
	}
	return observation, nil
}

func macOSIPv6LinkVerified(v guestObservation, before, after cage.GateStatus) bool {
	if !after.PolicyVerified || before.Mode != cage.GateChecking || after.Mode != cage.GateChecking || before.RunID == "" || after.RunID != before.RunID || before.CheckID == "" || after.CheckID != before.CheckID || after.IPv6LinkProbeCheck != before.CheckID {
		return false
	}
	if v.IPv6Connected || !v.IPv6UnscopedAbsent || !v.IPv6LinkSent || v.IPv6LinkInterface == "" || v.IPv6Unbound.Result != "no-route" || v.IPv6Unbound.GlobalSources != 0 || v.IPv6DefaultRoutes != len(v.IPv6Scoped) {
		return false
	}
	seen := map[string]bool{}
	for _, p := range v.IPv6Scoped {
		if !strings.HasPrefix(p.Name, "utun") || seen[p.Name] || !p.RouteVerified || p.BoundIndex <= 0 || p.GlobalSources != 0 || (p.Result != "no-route" && p.Result != "timeout") {
			return false
		}
		seen[p.Name] = true
	}
	return true
}

// Called only from the explicitly requested safety check. Restart the existing
// selected core without selecting another node or changing its revision.
func (s *Server) checkMacOSNetwork() error {
	cfg, err := loadConfig(s.configPath)
	if err != nil {
		return err
	}
	if err = cfg.RequireManagedProxy(s.profile); err != nil {
		return err
	}
	store, err := managedproxy.Load(s.profile)
	if err != nil {
		return err
	}
	if store.Selected == "" || store.Revision != cfg.ProxyRevision || store.Endpoint() != cfg.ProxyURL {
		return errors.New("请先为这台 macOS 客机选择并应用代理节点")
	}
	if s.vmRunning() {
		client, err := s.gateClient()
		if err != nil {
			return err
		}
		state, err := client.Status(s.operationContext())
		if err != nil {
			return err
		}
		if state.Configuration != cfg.Fingerprint() {
			return errors.New("网络设置已保存，请重启 macOS 客机，登录后重新检测")
		}
	}
	ctx := s.operationContext()
	if !managedproxy.Running(ctx, s.profile, store) {
		if err = validateProxyCore(ctx, s.profile, store); err != nil {
			return err
		}
		if err = startProxyCore(ctx, s.profile, store); err != nil {
			return err
		}
	}
	return s.runSafetyCheck()
}
