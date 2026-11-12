package workbench

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/diagnostics"
	"github.com/sylvinus/agent-vm/internal/usererror"
)

type networkRecovery struct {
	RunID, Configuration string
	PauseVersion         uint64
	Epoch                uint64
	Next                 time.Time
	Attempts             int
	pressureLogged       bool
}

// Called with s.mu held. Reuse the ordinary network status text on every
// platform; no independent timer or cached permission is introduced.
func (s *Server) networkRecoveryMessageLocked(now time.Time) string {
	r := s.recovery
	if r.RunID == "" || s.networkPaused || s.closing || s.safety.pauseVersion != r.PauseVersion || s.safety.Gate == nil || s.safety.Gate.RunID != r.RunID || s.safety.Gate.Configuration != r.Configuration || s.safety.Gate.Mode == cage.GateOpen {
		return ""
	}
	if s.safety.Gate.Mode == cage.GateClosed && s.safety.Gate.Epoch != r.Epoch {
		return "" // A newer closure must not promise this recovery will run.
	}
	job := s.jobs.snapshot()
	if s.safety.Gate.Mode != cage.GateClosed && (!job.Running || job.Kind != "network-recovery") {
		return "" // A manual check owns the next transition.
	}
	if job.Running {
		if job.Kind == "network-recovery" {
			return "正在执行自动完整重检，通过后恢复联网"
		}
		return "自动重检正在等待当前操作完成"
	}
	if !s.ready {
		return "自动重检正在等待环境就绪"
	}
	if now.Before(r.Next) {
		seconds := (r.Next.Sub(now) + time.Second - 1) / time.Second
		return fmt.Sprintf("自动重检将在 %d 秒后尝试", seconds)
	}
	if r.pressureLogged {
		return "自动重检正在等待内存压力缓解"
	}
	return "自动重检正在等待后台调度"
}

func (s *Server) scheduleNetworkRecovery(state cage.GateStatus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.networkPaused || s.closing || state.Mode != cage.GateClosed || state.Report == nil {
		return
	}
	s.recovery = networkRecovery{RunID: state.RunID, Configuration: state.Configuration, Epoch: state.Epoch, PauseVersion: s.safety.pauseVersion, Next: time.Now().Add(15 * time.Second)}
	diagnostics.Log("warn", "network", "temporary monitoring failure; full recheck scheduled: "+state.Reason)
}

func recoveryMatches(r networkRecovery, state cage.GateStatus, configuration string, paused bool, generation uint64) bool {
	return r.RunID != "" && !paused && generation == r.PauseVersion && state.RunID == r.RunID && state.Configuration == r.Configuration && configuration == r.Configuration && state.Epoch == r.Epoch && state.Mode == cage.GateClosed
}

// Called with s.op held. Admission uses the ordinary job queue and never changes
// saved network intent. The worker rechecks after acquiring s.op as well.
func (s *Server) recoverNetwork() {
	s.mu.Lock()
	r := s.recovery
	closing, paused, ready, jobRunning := s.closing, s.networkPaused, s.ready, s.jobs.snapshot().Running
	s.mu.Unlock()
	if r.RunID == "" {
		return
	}
	timing := s.startMonitorTiming()
	defer s.recordMonitorTiming("recovery", timing)
	switch {
	case closing || paused:
		timing.result = "cancelled"
		return
	case !ready:
		timing.result = "environment_not_ready"
		return
	case jobRunning:
		timing.result = "job_running"
		return
	case time.Now().Before(r.Next):
		timing.result = "retry_not_due"
		return
	}
	// The resource manager refreshes UpdatedAt even when a sample fails.
	// Only a recent pressure measurement should postpone the full gate check.
	resources, err := cage.ReadResourceStatus(s.instanceDir())
	guestPressure := resources.Pressure && time.Since(resources.ObservedAt) <= 10*time.Second
	if err == nil && (guestPressure || resources.HostPressure) {
		timing.result = "memory_pressure"
		s.mu.Lock()
		logWait := s.recovery == r && !r.pressureLogged
		if logWait {
			s.recovery.pressureLogged = true
		}
		s.mu.Unlock()
		if logWait {
			diagnostics.Log("info", "network", fmt.Sprintf("automatic recheck waiting for memory pressure to subside (guest=%t, host=%t)", guestPressure, resources.HostPressure))
		}
		return
	}
	_, _ = s.submitJob("network-recovery", func() error { return s.runNetworkRecovery(r, s.runSafetyCheck) })
}

func (s *Server) runNetworkRecovery(expected networkRecovery, recheck func() error) error {
	s.mu.Lock()
	stopped := s.closing || s.networkPaused || s.safety.pauseVersion != expected.PauseVersion
	if stopped {
		s.recovery = networkRecovery{}
	}
	s.mu.Unlock()
	if stopped {
		return context.Canceled
	}
	client, err := s.gateClient()
	if err != nil {
		return s.finishNetworkRecovery(expected, err)
	}
	ctx, cancel := context.WithTimeout(s.operationContext(), 5*time.Second)
	state, err := client.Status(ctx)
	cancel()
	if err != nil {
		return s.finishNetworkRecovery(expected, err)
	}
	cfg, err := loadConfig(s.configPath)
	if err != nil {
		return s.finishNetworkRecovery(expected, err)
	}
	s.mu.Lock()
	valid := !s.closing && s.recovery == expected && recoveryMatches(expected, state, cfg.Fingerprint(), s.networkPaused, s.safety.pauseVersion)
	if !valid {
		s.recovery = networkRecovery{}
	}
	s.mu.Unlock()
	if !valid {
		return context.Canceled
	}
	if err := s.operationContext().Err(); err != nil {
		return err
	}
	s.setMessage("正在重新检查环境与出口，通过后自动恢复联网…")
	err = recheck()
	return s.finishNetworkRecovery(expected, err)
}

func (s *Server) finishNetworkRecovery(expected networkRecovery, err error) error {
	// A failed full check changes the gate epoch. Keep that exact epoch for the
	// next attempt, while preserving user/configuration changes as cancellation.
	var current cage.GateStatus
	if err != nil {
		if client, e := s.gateClient(); e == nil {
			ctx, cancel := context.WithTimeout(s.lifetimeContext(), 3*time.Second)
			current, _ = client.Status(ctx)
			cancel()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recovery != expected {
		return err
	}
	if err == nil || s.closing || s.networkPaused || s.safety.pauseVersion != expected.PauseVersion || errors.Is(err, context.Canceled) {
		s.recovery = networkRecovery{}
		return err
	}
	if current.RunID != "" {
		if current.RunID != expected.RunID || current.Configuration != expected.Configuration || current.Mode != cage.GateClosed {
			s.recovery = networkRecovery{}
			return err
		}
		s.recovery.Epoch = current.Epoch
	}
	s.recovery.Attempts++
	delay := time.Duration(min(300, 15<<min(s.recovery.Attempts, 5))) * time.Second
	s.recovery.Next = time.Now().Add(delay)
	s.recovery.pressureLogged = false
	// Show the latest full-check failure rather than the initial monitoring
	// error, which may describe a different missing sample by this point.
	s.safety.monitorError = diagnostics.Redact(usererror.Display("自动完整重检", err, "等待下一次重检；仍失败时导出诊断包。").Error())
	diagnostics.Log("warn", "network", fmt.Sprintf("automatic recheck attempt %d failed; next attempt in %s", s.recovery.Attempts, delay))
	return err
}
