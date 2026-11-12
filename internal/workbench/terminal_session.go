package workbench

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
)

var errTerminalInputFull = errors.New("terminal input queue is full")

type terminalPTY interface {
	io.ReadWriteCloser
	Resize(cols, rows uint16) error
	Wait() error
	Kill() error
}

type terminalSession struct {
	mu                    sync.Mutex
	id, title             string
	sharedID              string
	connectionID          string
	conversationEnded     bool
	workspace             string
	kind, claudeAccountID string
	loginGuestID          string
	loginLinkMu           sync.Mutex
	loginLinkCleanup      sync.Once
	claudeAccountSource   string
	cmd                   *exec.Cmd
	file                  terminalPTY
	history               []byte
	closed                bool
	clients               map[chan []byte]bool
	done                  chan struct{}
	inputs                chan string
	stopping              chan struct{}
	closeOnce             sync.Once
	cliStartupDone        chan struct{}
}

func (t *terminalSession) ownsConversation() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return !t.closed && !t.conversationEnded
}

func newTerminalSession(file terminalPTY) *terminalSession {
	return &terminalSession{file: file, clients: map[chan []byte]bool{}, done: make(chan struct{}), inputs: make(chan string, 8), stopping: make(chan struct{})}
}

func (t *terminalSession) summary() map[string]any {
	t.mu.Lock()
	defer t.mu.Unlock()
	closed := t.closed
	if t.done != nil {
		select {
		case <-t.done:
			closed = true
		default:
			closed = false
		}
	}
	return map[string]any{"id": t.id, "title": t.title, "closed": closed, "workspace": t.workspace, "kind": t.kind,
		"claude_account_id": t.claudeAccountID, "claude_account_source": t.claudeAccountSource, "cli_session_id": t.sharedID,
		"cli_starting": t.cliStartupPendingLocked()}
}

func (t *terminalSession) cliStartupPendingLocked() bool {
	if t.cliStartupDone == nil || t.closed {
		return false
	}
	select {
	case <-t.cliStartupDone:
		return false
	default:
		return true
	}
}

func (t *terminalSession) finishCLIStartup() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cliStartupPendingLocked() {
		close(t.cliStartupDone)
	}
}

func (t *terminalSession) stopPTY() {
	t.closeOnce.Do(func() {
		t.mu.Lock()
		t.closed = true
		t.mu.Unlock()
		if t.stopping != nil {
			close(t.stopping)
		}
		if t.file != nil {
			// Close may wait inside ConPTY. Never hold the session mutex while
			// the reader needs it to drain the final output and finish.
			_ = t.file.Kill()
			_ = t.file.Close()
		}
	})
}

func (t *terminalSession) close() {
	t.stopPTY()
	if t.done != nil {
		<-t.done
	}
}

// Input belongs to the session once admitted. A viewer never waits inside a
// PTY write, so disconnecting or locking the account can drain its handler
// while a guest process is not reading input. A full queue rejects the viewer
// immediately, so the handler never waits to read a WebSocket close either.
func (t *terminalSession) enqueueInput(ctx context.Context, input string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return os.ErrClosed
	}
	select {
	case t.inputs <- input:
		return nil
	default:
		return errTerminalInputFull
	}
}

func (t *terminalSession) writeInput() {
	for {
		select {
		case <-t.stopping:
			return
		case input := <-t.inputs:
			if _, err := io.WriteString(t.file, input); err != nil {
				t.stopPTY()
				return
			}
		}
	}
}

type terminalSubscription struct {
	History     []byte
	Output      <-chan []byte
	CLIStartup  <-chan struct{}
	CLIStarting bool
	session     *terminalSession
	channel     chan []byte
}

// subscribe captures history and registers live output under the same lock.
// Every byte belongs to exactly one side of this history/live boundary.
func (t *terminalSession) subscribe() *terminalSubscription {
	t.mu.Lock()
	defer t.mu.Unlock()
	channel := make(chan []byte, 64)
	if t.closed {
		close(channel)
	} else {
		t.clients[channel] = true
	}
	history := append([]byte(nil), t.history...)
	if t.kind == "claude-login" {
		history = []byte("请在 Cage 浏览器的临时登录页完成授权。若页面提供验证码，可返回此终端输入。\r\n")
	}
	return &terminalSubscription{History: history, Output: channel, CLIStartup: t.cliStartupDone,
		CLIStarting: t.cliStartupPendingLocked(), session: t, channel: channel}
}

// Close detaches the viewer; the PTY and its process continue running.
func (s *terminalSubscription) Close() {
	s.session.mu.Lock()
	defer s.session.mu.Unlock()
	if s.session.clients[s.channel] {
		delete(s.session.clients, s.channel)
		close(s.channel)
	}
}

func (t *terminalSession) run() {
	writerDone := make(chan struct{})
	go func() { defer close(writerDone); t.writeInput() }()
	t.read()
	// An SSH process can remain alive after its output side closes. Once no
	// output can be read, terminate and reap it before completing the session.
	t.stopPTY()
	_ = t.file.Wait()
	<-writerDone
	// Admission is sealed under mu before stopping the writer. Release queued
	// input even when the closed terminal remains visible with its history.
	for len(t.inputs) > 0 {
		<-t.inputs
	}
	t.mu.Lock()
	for channel := range t.clients {
		close(channel)
		delete(t.clients, channel)
	}
	t.mu.Unlock()
	close(t.done)
}

func (t *terminalSession) read() {
	buffer := make([]byte, 16<<10)
	for {
		n, err := t.file.Read(buffer)
		if n > 0 {
			chunk := append([]byte(nil), buffer[:n]...)
			t.mu.Lock()
			if t.kind != "claude-login" {
				t.history = append(t.history, chunk...)
				if len(t.history) > 512<<10 {
					t.history = t.history[len(t.history)-(512<<10):]
				}
			}
			for channel := range t.clients {
				select {
				case channel <- chunk:
				default:
					close(channel)
					delete(t.clients, channel)
				}
			}
			t.mu.Unlock()
		}
		if err != nil {
			break
		}
	}
}
