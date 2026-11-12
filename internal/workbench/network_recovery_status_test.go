package workbench

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/cage"
)

func TestFailedNetworkRecoveryShowsLatestCheckReason(t *testing.T) {
	s, _ := failedMonitoringServer(t)
	err := s.runNetworkRecovery(s.recovery, func() error {
		return errors.New("出口采样不完整：浏览器没有收到检测结果")
	})
	message := s.currentNetworkErrorLocked()
	if err == nil || !strings.Contains(message, "浏览器没有收到检测结果") || !strings.Contains(message, "自动重检将在") || strings.Contains(message, "guest observation timed out") {
		t.Fatalf("latest failure or next retry was hidden: %s error=%v", message, err)
	}
}

func TestNetworkRecoveryStatusExplainsWaitWithoutChangingPermission(t *testing.T) {
	s, gate := failedMonitoringServer(t)
	now := time.Now()
	s.recovery.Next = now.Add(1500 * time.Millisecond)
	before := gate.Status()
	if message := s.networkRecoveryMessageLocked(now); !strings.Contains(message, "2 秒") {
		t.Fatal("missing bounded countdown", message)
	}
	if message := s.networkRecoveryMessageLocked(now.Add(2 * time.Second)); !strings.Contains(message, "后台调度") {
		t.Fatal("missing scheduling wait", message)
	}
	s.recovery.Next = now.Add(-time.Second)
	s.recovery.pressureLogged = true
	if message := s.networkRecoveryMessageLocked(now); !strings.Contains(message, "内存压力") {
		t.Fatal("missing memory wait", message)
	}
	s.setJobForTest(Job{Kind: "environment-tools", Running: true})
	if message := s.networkRecoveryMessageLocked(now); !strings.Contains(message, "当前操作") {
		t.Fatal("missing operation wait", message)
	}
	s.setJobForTest(Job{Kind: "network-recovery", Running: true})
	if message := s.networkRecoveryMessageLocked(now); !strings.Contains(message, "完整重检") {
		t.Fatal("missing recovery progress", message)
	}
	s.setJobForTest(Job{})
	checking, err := s.gateClient()
	if err != nil {
		t.Fatal(err)
	}
	state, err := checking.BeginCheck(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	s.publishGate(state)
	if message := s.networkRecoveryMessageLocked(now); message != "" {
		t.Fatal("automatic countdown overwrote a manual check", message)
	}
	gate.Close("test closed")
	s.publishGate(gate.Status())
	if message := s.networkRecoveryMessageLocked(now); message != "" {
		t.Fatal("a new closure retained a stale recovery promise", message)
	}
	s.networkPaused = true
	if message := s.networkRecoveryMessageLocked(now); message != "" {
		t.Fatal("paused environment promised automatic recovery", message)
	}
	if after := gate.Status(); after.Mode != cage.GateClosed || after.Until.After(before.Until) {
		t.Fatal("status text changed permission")
	}
}
