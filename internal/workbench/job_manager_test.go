package workbench

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func awaitJobSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal("job did not reach expected state")
	}
}

func awaitJobIdle(t *testing.T, manager *jobManager) Job {
	t.Helper()
	deadline := time.After(3 * time.Second)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		state := manager.snapshot()
		if !state.Running {
			return state
		}
		select {
		case <-deadline:
			t.Fatal("job remained running")
		case <-tick.C:
		}
	}
}

func TestJobManagerStopPreemptsAndFencesOldExecution(t *testing.T) {
	var manager jobManager
	var operation sync.Mutex
	defer manager.close()
	started, canceled, stopped := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var old *jobExecution
	first, err := manager.submit(&operation, "start", 17, nil, nil, func() error {
		old = manager.execution()
		close(started)
		<-old.ctx.Done()
		old.message("stale message")
		_, _ = old.Write([]byte("stale output"))
		close(canceled)
		return errors.New("stale error")
	})
	if err != nil {
		t.Fatal(err)
	}
	awaitJobSignal(t, started)
	if old.id != first.ID || old.pauseVersion != 17 {
		t.Fatal("execution lost admission identity or pause generation")
	}
	if _, err := manager.submit(&operation, "settings", 0, func() error {
		t.Error("rejected job prepared side effects")
		return nil
	}, nil, func() error { return nil }); !errors.Is(err, errJobBusy) {
		t.Fatalf("busy job accepted: %v", err)
	}
	stop, err := manager.submit(&operation, "stop", 18, nil, nil, func() error {
		close(stopped)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	awaitJobSignal(t, canceled)
	awaitJobSignal(t, stopped)
	state := awaitJobIdle(t, &manager)
	if state.ID != stop.ID || state.Error != "" || state.Log != "" || state.Message != "已完成" {
		t.Fatalf("old execution changed stop result: %+v", state)
	}

	// Retained writers must stay tied to their original job after later work
	// starts. Looking up the current execution on every Write would fail here.
	startedAgain, release := make(chan struct{}), make(chan struct{})
	_, err = manager.submit(&operation, "settings", 19, nil, nil, func() error {
		close(startedAgain)
		<-release
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	awaitJobSignal(t, startedAgain)
	old.message("late message")
	_, _ = old.Write([]byte("late output"))
	state = manager.snapshot()
	close(release)
	if state.Log != "" || state.Message != "正在准备…" {
		t.Fatalf("late output reached another job: %+v", state)
	}
}

func TestJobManagerCancellationWhileWaitingFinishesState(t *testing.T) {
	var manager jobManager
	var operation sync.Mutex
	operation.Lock()
	var ran atomic.Bool
	_, err := manager.submit(&operation, "start", 0, nil, nil, func() error {
		ran.Store(true)
		return nil
	})
	if err != nil {
		operation.Unlock()
		t.Fatal(err)
	}
	manager.cancelRunning()
	operation.Unlock()
	defer manager.close()
	state := awaitJobIdle(t, &manager)
	if ran.Load() || state.Message != "已取消" || state.Error != "" {
		t.Fatalf("cancelled waiting job executed or stayed running: %+v", state)
	}
}

func TestJobManagerRepeatedStopCanRequestLogout(t *testing.T) {
	for _, stopFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[stopFails], func(t *testing.T) {
			var manager jobManager
			var operation sync.Mutex
			defer manager.close()
			started, release := make(chan struct{}), make(chan struct{})
			var locked atomic.Int32
			first, err := manager.submit(&operation, "stop", 0, nil, nil, func() error {
				close(started)
				<-release
				if stopFails {
					return errors.New("测试模拟：环境停止失败")
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			awaitJobSignal(t, started)
			second, err := manager.submit(&operation, "stop", 1, func() error {
				t.Error("duplicate stop ran prepare")
				return nil
			}, func() error { locked.Add(1); return nil }, func() error {
				t.Error("duplicate stop ran twice")
				return nil
			})
			if err != nil || second.ID != first.ID {
				close(release)
				t.Fatalf("stop was not joined: %+v %v", second, err)
			}
			// A later ordinary stop must preserve the joined logout request.
			_, err = manager.submit(&operation, "stop", 2, nil, nil, func() error { return nil })
			close(release)
			if err != nil {
				t.Fatal(err)
			}
			state := awaitJobIdle(t, &manager)
			if stopFails {
				if locked.Load() != 0 || state.Error != "测试模拟：环境停止失败" {
					t.Fatalf("failed stop locked account: %+v count=%d", state, locked.Load())
				}
			} else if locked.Load() != 1 || state.Error != "" {
				t.Fatalf("joined logout not completed once: %+v count=%d", state, locked.Load())
			}
		})
	}
}

func TestJobManagerCloseWaitsForCanceledAndSupersededWork(t *testing.T) {
	var manager jobManager
	var operation sync.Mutex
	started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	_, err := manager.submit(&operation, "start", 0, nil, nil, func() error {
		execution := manager.execution()
		close(started)
		<-execution.ctx.Done()
		close(canceled)
		<-release
		return execution.ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	awaitJobSignal(t, started)
	var stopRan atomic.Bool
	_, err = manager.submit(&operation, "stop", 0, nil, nil, func() error {
		stopRan.Store(true)
		return nil
	})
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	awaitJobSignal(t, canceled)
	closed := make(chan struct{})
	go func() { manager.close(); close(closed) }()
	// Wait for admission to be sealed while the old worker still owns the
	// operation lock. Closing must not report completion at this point.
	deadline := time.Now().Add(3 * time.Second)
	for {
		manager.mu.Lock()
		sealed := manager.closed
		manager.mu.Unlock()
		if sealed {
			break
		}
		if time.Now().After(deadline) {
			close(release)
			t.Fatal("close did not seal admission")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-closed:
		close(release)
		t.Fatal("close returned before old work ended")
	default:
	}
	_, err = manager.submit(&operation, "start", 0, nil, nil, func() error { return nil })
	if !errors.Is(err, errJobsClosed) {
		close(release)
		t.Fatalf("job admitted during close: %v", err)
	}
	closedAgain := make(chan struct{})
	go func() { manager.close(); close(closedAgain) }()
	close(release)
	awaitJobSignal(t, closed)
	awaitJobSignal(t, closedAgain)
	state := manager.snapshot()
	if stopRan.Load() || state.Running || state.Message != "已取消" {
		t.Fatalf("queued stop ran after shutdown: %+v", state)
	}
}

func TestJobManagerFinalizationAndOutput(t *testing.T) {
	var manager jobManager
	var operation sync.Mutex
	defer manager.close()
	outside := manager.execution()
	_, _ = outside.Write([]byte("unowned output"))
	outside.message("unowned message")
	if manager.snapshot() != (Job{}) {
		t.Fatal("output without an execution changed state")
	}
	_, err := manager.submit(&operation, "stop", 0, nil, func() error {
		return errors.New("无法写入账户锁定状态")
	}, func() error {
		_, _ = manager.execution().Write([]byte(strings.Repeat("a\n", 12500) + "tail"))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	state := awaitJobIdle(t, &manager)
	if state.Error != "无法写入账户锁定状态" || len(state.Log) > 24000 || !strings.Contains(state.Log, "12499 additional times") || !strings.HasSuffix(state.Log, "tail\n") {
		t.Fatalf("finalization error or bounded log lost: %+v", state)
	}
	_, err = manager.submit(&operation, "stop", 0, nil, nil, func() error {
		var output strings.Builder
		for i := 0; i < 4000; i++ {
			fmt.Fprintf(&output, "different line %d\n", i)
		}
		_, _ = manager.execution().Write([]byte(output.String()))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	state = awaitJobIdle(t, &manager)
	if len(state.Log) != 24000 || !strings.HasSuffix(state.Log, "different line 3999\n") {
		t.Fatal("distinct output lost its bound or final line")
	}
}

func TestJobManagerRejectedPreparationDoesNotPublishJob(t *testing.T) {
	var manager jobManager
	var operation sync.Mutex
	_, err := manager.submit(&operation, "start", 0, func() error { return context.Canceled }, nil, func() error {
		t.Error("unprepared job ran")
		return nil
	})
	manager.close()
	if !errors.Is(err, context.Canceled) || manager.snapshot() != (Job{}) {
		t.Fatalf("failed preparation published a job: %+v %v", manager.snapshot(), err)
	}
}

// The initialization view needs the failed prerequisite even when no project
// error snapshot is available. This also survives wrapped lifecycle errors.
func TestJobManagerPreservesProjectRecovery(t *testing.T) {
	var manager jobManager
	var operation sync.Mutex
	defer manager.close()
	_, err := manager.submit(&operation, "safety", 0, nil, nil, func() error {
		return fmt.Errorf("network prerequisite: %w", &projectShareError{Path: "/shared/workspace", Recovery: "project-approval", Message: "请允许工作区访问"})
	})
	if err != nil {
		t.Fatal(err)
	}
	job := awaitJobIdle(t, &manager)
	if job.Recovery != "project-approval" || job.Workspace != "/shared/workspace" || !strings.Contains(job.Error, "请允许工作区访问") {
		t.Fatalf("lost actionable failure: %+v", job)
	}
	_, err = manager.submit(&operation, "safety", 0, nil, nil, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	job = awaitJobIdle(t, &manager)
	if job.Error != "" || job.Recovery != "" || job.Workspace != "" {
		t.Fatalf("stale recovery after retry: %+v", job)
	}
}
