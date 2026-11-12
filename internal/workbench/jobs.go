package workbench

import (
	"context"
	"errors"
	"fmt"
	"github.com/sylvinus/agent-vm/internal/diagnostics"
	"github.com/sylvinus/agent-vm/internal/usererror"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

type Job struct {
	Canceled      bool           `json:"canceled,omitempty"`
	SettingsSaved bool           `json:"settings_saved,omitempty"`
	SetupProgress *SetupProgress `json:"setup_progress,omitempty"`
	ID            string         `json:"id"`
	Kind          string         `json:"kind"`
	Running       bool           `json:"running"`
	Message       string         `json:"message"`
	Error         string         `json:"error,omitempty"`
	Recovery      string         `json:"recovery,omitempty"`
	Workspace     string         `json:"workspace,omitempty"`
	Log           string         `json:"log,omitempty"`
}

var (
	errJobBusy    = errors.New("另一个操作仍在进行，请稍候")
	errJobsClosed = errors.New("后台正在更新，请重新打开 Cage。")
)

// jobManager owns accepted work independently of HTTP requests. The operation
// lock is shared with synchronous workbench operations and monitoring.
type jobManager struct {
	mu           sync.Mutex
	state        Job
	cancel       context.CancelFunc
	active       *jobExecution
	afterSuccess func() error
	closed       bool
	workers      sync.WaitGroup
}

// prepare runs synchronously only for a new admission. Neither prepare nor
// afterSuccess may call this manager. afterSuccess must not acquire Server.mu:
// submission can hold Server.mu while waiting for the manager's lock.
func (m *jobManager) submit(operation sync.Locker, kind string, pauseVersion uint64, prepare func() error, afterSuccess func() error, run func() error) (Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return Job{}, errJobsClosed
	}
	if m.state.Running {
		if kind == "stop" && m.state.Kind == "stop" {
			// Logout can join a pending stop and request account locking once
			// that same stop succeeds. A plain repeated stop cannot undo it.
			if afterSuccess != nil {
				m.afterSuccess = afterSuccess
			}
			return m.state, nil
		}
		if kind != "stop" {
			return Job{}, fmt.Errorf("%w：当前正在%s。请等待完成，或先停止当前操作。", errJobBusy, jobName(m.state.Kind))
		}
	}
	if prepare != nil {
		if err := prepare(); err != nil {
			return Job{}, err
		}
	}
	if m.cancel != nil {
		m.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.afterSuccess = afterSuccess
	m.state = Job{ID: randomToken()[:12], Kind: kind, Running: true, Message: "正在准备…"}
	execution := &jobExecution{owner: m, id: m.state.ID, ctx: ctx, pauseVersion: pauseVersion}
	execution.log = diagnostics.NewStream(diagnostics.Default(), "task/"+kind, "info", func(line string) {
		if m.state.ID == execution.id {
			if m.state.SetupProgress != nil {
				if p := parseSetupProgress(line); p != nil {
					previous := m.state.SetupProgress
					if p.System == "" {
						p.System = previous.System
					}
					now := time.Now()
					if p.Operation != "verify" && p.Label == previous.Label && p.Total == previous.Total && p.Received != nil && previous.Received != nil && *p.Received >= *previous.Received && !execution.progressUpdated.IsZero() {
						p.BytesPerSecond = float64(*p.Received-*previous.Received) / now.Sub(execution.progressUpdated).Seconds()
					}
					execution.progressUpdated = now
					m.state.SetupProgress = p
					if strings.HasPrefix(line, "CAGE_") {
						return
					}
				}
			}
			m.state.Log += line
			if len(m.state.Log) > 24000 {
				m.state.Log = m.state.Log[len(m.state.Log)-24000:]
			}
		}
	})
	diagnostics.Log("info", "task", kind+" started")
	m.workers.Add(1)
	go func() {
		defer m.workers.Done()
		defer cancel()
		operation.Lock()
		defer operation.Unlock()
		m.mu.Lock()
		if m.state.ID != execution.id {
			m.mu.Unlock()
			return
		}
		if ctx.Err() != nil {
			m.finishLocked(execution, ctx.Err())
			m.mu.Unlock()
			return
		}
		m.active = execution
		m.mu.Unlock()
		err := func() (err error) {
			defer func() {
				if failure := recover(); failure != nil {
					diagnostics.Log("error", "task", fmt.Sprintf("panic: %v\n%s", failure, debug.Stack()))
					panic(failure)
				}
			}()
			return run()
		}()
		m.mu.Lock()
		defer m.mu.Unlock()
		m.finishLocked(execution, err)
	}()
	return m.state, nil
}

func (m *jobManager) finishLocked(execution *jobExecution, err error) {
	if execution.log != nil {
		_ = execution.log.Close()
	}
	if m.active == execution {
		m.active = nil
	}
	if m.state.ID != execution.id {
		return
	}
	if err == nil {
		err = execution.ctx.Err()
	}
	if err == nil && m.afterSuccess != nil {
		err = m.afterSuccess()
	}
	m.cancel, m.afterSuccess = nil, nil
	m.state.Running = false
	switch {
	case errors.Is(err, context.Canceled):
		m.state.Canceled = true
		m.state.Message = "已取消"
		if m.state.SettingsSaved {
			m.state.Message = "设置已保存，后续操作已取消"
		}
		diagnostics.Log("info", "task", m.state.Kind+" canceled")
	case err != nil:
		m.state.Error = usererror.Display(jobName(m.state.Kind), err, "打开“操作详情”查看失败记录；仍无法处理时，"+failureHelp).Error()
		var projectFailure *projectShareError
		if errors.As(err, &projectFailure) {
			m.state.Recovery = projectFailure.Recovery
			m.state.Workspace = projectFailure.Path
		}
		m.state.Message = jobName(m.state.Kind) + "失败"
		var networkFailure networkCheckError
		if m.state.Kind == "start" && errors.As(err, &networkFailure) {
			m.state.Message = "环境已启动，联网检查未通过"
		} else if m.state.SettingsSaved {
			m.state.Message = "设置已保存，等待环境恢复"
			if errors.As(err, &networkFailure) {
				m.state.Message = "设置已保存，环境已启动，联网检查未通过"
			}
		}
		diagnostics.Log("error", "task", fmt.Sprintf("%s: %v; cause=%v", m.state.Kind, err, errors.Unwrap(err)))
	default:
		m.state.Message = "已完成"
		diagnostics.Log("info", "task", m.state.Kind+" completed")
	}
}

func jobName(kind string) string {
	names := map[string]string{
		"start": "启动环境", "stop": "停止环境", "setup": "准备环境", "settings": "应用设置", "project": "连接项目目录", "storage": "迁移环境目录", "restore": "恢复配置", "resources": "应用资源配置", "safety": "检查联网条件", "network-recovery": "恢复联网", "proxy": "应用代理节点", "proxy-network": "恢复代理连接", "environment-update": "更新环境组件", "environment-tools": "更新环境工具", "claude-desktop-install": "安装 Claude Desktop", "claude-desktop-start": "打开 Claude Desktop", "claude-desktop-stop": "关闭 Claude Desktop",
	}
	if name := names[kind]; name != "" {
		return name
	}
	return "本次操作"
}

func (m *jobManager) snapshot() Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

// execution returns the operation that is actually running, including an old
// operation that has been cancelled by a stop but has not returned yet.
func (m *jobManager) execution() *jobExecution {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active != nil {
		return m.active
	}
	return &jobExecution{owner: m, ctx: context.Background()}
}

func (m *jobManager) cancelRunning() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state.Kind != "stop" && m.cancel != nil {
		m.cancel()
	}
	if m.active != nil {
		return m.active.id
	}
	return ""
}

// close seals admission before waiting, so no worker can be added concurrently
// with Wait. Superseded workers are included even while waiting for operation.
func (m *jobManager) close() {
	m.mu.Lock()
	m.closed = true
	if m.cancel != nil {
		m.cancel()
	}
	m.mu.Unlock()
	m.workers.Wait()
}

type jobExecution struct {
	owner           *jobManager
	id              string
	ctx             context.Context
	pauseVersion    uint64
	log             *diagnostics.Stream
	progressUpdated time.Time
}

func (e *jobExecution) message(message string) {
	e.owner.mu.Lock()
	defer e.owner.mu.Unlock()
	if e.id != "" && e.owner.state.ID == e.id && e.owner.state.Running {
		if e.owner.state.SettingsSaved {
			message = "设置已保存，" + message
		}
		e.owner.state.Message = message
	}
}

func (e *jobExecution) settingsSaved() {
	e.owner.mu.Lock()
	defer e.owner.mu.Unlock()
	if e.id != "" && e.owner.state.ID == e.id && e.owner.state.Running {
		e.owner.state.SettingsSaved = true
	}
}

func (e *jobExecution) Write(data []byte) (int, error) {
	e.owner.mu.Lock()
	defer e.owner.mu.Unlock()
	if e.id != "" && e.owner.state.ID == e.id && e.owner.state.Running {
		if e.log != nil {
			_, _ = e.log.Write(data)
		}
	}
	return len(data), nil
}
