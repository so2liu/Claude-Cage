package workbench

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"al.essio.dev/pkg/shellescape"

	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/guestos"
	"github.com/sylvinus/agent-vm/internal/host"
)

// Only the trusted local workbench can operate the native Mac. Each route
// must have a native guest implementation or run entirely on the host.
func macOSWorkbenchRoute(path string) bool {
	if nativeWorkbenchRoute(path) {
		return true
	}
	if id, ok := strings.CutPrefix(path, "/api/input/files/desktop/"); ok {
		return id == "requests" || id == "open" || browserFileID.MatchString(id)
	}
	switch path {
	case "/api/macos/desktop", "/api/macos/first-setup", "/api/appearance/codex", "/api/macos/project-approval", "/api/macos/exit-policy", "/api/macos/login", "/api/macos/maintenance":
		return true
	}
	return strings.HasPrefix(path, "/api/macos/chrome/") || strings.HasPrefix(path, "/api/macos/claude/") || strings.HasPrefix(path, "/api/macos-browser/")
}

func macOSInstanceRunning(instanceDir string) bool {
	data, err := os.ReadFile(filepath.Join(instanceDir, "cage-running.json"))
	var record struct {
		PID     int    `json:"pid"`
		Started string `json:"started"`
	}
	if err != nil || json.Unmarshal(data, &record) != nil || record.PID < 1 {
		return false
	}
	return processExists(record.PID) && cage.ProcessStartMatches(record.PID, record.Started)
}

func (s *Server) macOSSSHCommand(ctx context.Context, args ...string) *exec.Cmd {
	base, err := guestos.MacOSSSHArgs(s.profile, s.macosGuest.User)
	if err != nil {
		return &exec.Cmd{Err: err}
	}
	cmd := exec.CommandContext(ctx, sshExecutable(), append(base, args...)...)
	cmd.Env = sshEnvironment()
	host.HideCommandWindow(cmd)
	return cmd
}

func (s *Server) checkMacOSGuest(ctx context.Context) error {
	if !s.vmRunning() {
		return errors.New("macOS 客机尚未启动")
	}
	client, err := s.gateClient()
	if err != nil {
		return errors.New("无法验证 macOS 客机的网络隔离状态")
	}
	state, err := client.Status(ctx)
	if err != nil {
		return errors.New("无法验证 macOS 客机的网络门槛")
	}
	if state.Mode != cage.GateClosed {
		s.mu.Lock()
		ready, paused := s.ready, s.networkPaused
		s.mu.Unlock()
		reason := ""
		switch {
		case s.macSetupPending():
			reason = "首次系统设置期间外网关闭"
		case paused:
			reason = "网络保持暂停，请手动恢复后再联网"
		case !ready:
			reason = "客机重新连接，外网等待重新检查"
		default:
			cfg, cfgErr := loadConfig(s.configPath)
			if state.Mode != cage.GateOpen || cfgErr != nil || state.Report == nil || !time.Now().Before(state.Until) || state.Report.Authorizes(state.RunID, cfg.Fingerprint(), time.Now()) != nil {
				reason = "网络授权未通过当前配置的完整检查"
			}
		}
		if reason != "" {
			state, err = client.Close(ctx, reason)
			if err != nil || state.Mode != cage.GateClosed {
				return errors.New("macOS 客机网络状态不能确认")
			}
		}
	}
	s.publishGate(state)
	probe := `test "$(uname -s)" = Darwin && [[ $(sysctl -n hw.model) == VirtualMac* ]] && test "$(id -un)" = ` +
		shellescape.Quote(s.macosGuest.User) + ` && [[ " $(id -Gn) " != *" admin "* ]]`
	if _, err := s.guest(ctx, probe); err != nil {
		return errors.New("无法以已绑定的普通用户连接到这台虚拟 Mac，请检查客机身份和 SSH 登录")
	}
	return nil
}

func (s *Server) attachMacOSGuest() error {
	ctx, cancel := s.controlContext()
	defer cancel()
	err := s.checkMacOSGuest(ctx)
	s.mu.Lock()
	newlyReady := !s.ready && err == nil
	checkAfterReconnect := newlyReady && !s.closing && !s.networkPaused && !s.jobs.snapshot().Running
	s.ready = err == nil
	s.guestUpdateError = ""
	if err != nil {
		s.guestUpdateError = err.Error()
	}
	s.mu.Unlock()
	if newlyReady && !s.macSetupPending() {
		s.restoreProjectShares()
		if checkAfterReconnect {
			_, _ = s.submitJob("network-recovery", s.checkNetworkIfWanted)
		}
	} else if err == nil && !s.macSetupPending() {
		s.retryMacOSProjectShares(ctx)
	}
	return err
}

func (s *Server) macOSLifecycleCommand(action string) error {
	ctx, cancel := context.WithTimeout(s.operationContext(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.exe, "guest-os", "macos", action)
	cmd.Env = append(os.Environ(), cage.ConfigEnv+"="+s.configPath)
	job := s.jobs.execution()
	cmd.Stdout, cmd.Stderr = job, job
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("macOS 客机操作失败（%s）：%w", action, err)
	}
	return nil
}

func (s *Server) startMacOSGuest() error {
	if !s.vmRunning() {
		if err := s.macOSLifecycleCommand("start"); err != nil {
			return err
		}
	}
	if err := s.syncMacGuestPassword(s.operationContext()); err != nil {
		return err
	}
	if err := s.attachMacOSGuest(); err != nil {
		return err
	}
	cfg, err := loadConfig(s.configPath)
	if err != nil {
		return err
	}
	if err := s.closeProxyGate("正在应用 macOS 系统设置，外网等待重新检查"); err != nil {
		return err
	}
	if err := s.applyMacOSGuestSettings(s.operationContext(), cfg); err != nil {
		return err
	}
	return s.checkNetworkIfWanted()
}

//go:embed macos-locale.sh
var macOSLocaleScript string

func (s *Server) syncMacOSRegion(ctx context.Context, cfg cage.Config) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	s.setMessage("正在应用 macOS 客机账户的地区与语言…")
	locale := strings.TrimSuffix(cfg.Locale, ".UTF-8")
	cmd := shellescape.QuoteCommand([]string{"/bin/bash", "-c", macOSLocaleScript, "cage-locale", "NSGlobalDomain", locale})
	if _, err := s.guest(ctx, "exec "+cmd); err != nil {
		return fmt.Errorf("macOS 客机已启动，未能写入客机账户的地区与语言：%w", err)
	}
	s.setMessage("正在等候 macOS 客机读回已保存的时区与语言…")
	var last guestObservation
	for {
		got, err := s.macOSGuestObservation(ctx, map[string]any{"stage": "environment"})
		if err == nil {
			last = got
			if s.guestSettingsOK(got, cfg) {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.Canceled) {
				return ctx.Err()
			}
			return fmt.Errorf("macOS 客机的时区和语言尚未全部生效。期望 %s、%s，系统读回时区 %q、地区 %q、首选语言 %q，进程读回 %q、%q。外网保持关闭。可稍后重检，时区持续不符时请停止并重新启动客机。", cfg.Timezone, cfg.Locale, last.SystemTimezone, last.SystemLocale, last.SystemLanguage, last.TZ, last.ProcessLocale)
		case <-time.After(time.Second):
		}
	}
}

func (s *Server) stopMacOSGuest() error {
	s.mu.Lock()
	s.macSetupProjectsChecked = time.Time{}
	s.mu.Unlock()
	s.closeWindowsBrowser()
	s.mu.Lock()
	s.ready = false
	s.mu.Unlock()
	s.terminals.closeAll()
	s.closeProjectShares()
	return s.macOSLifecycleCommand("stop")
}
