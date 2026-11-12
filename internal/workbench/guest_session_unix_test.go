//go:build !windows

package workbench

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// The server that runs on the guest, started here as a local process.
func localGuestSession(t *testing.T) (run func(context.Context, string) (guestSessionReply, bool, error), session *guestSession, starts *atomic.Int32) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the control session serves Linux guests")
	}
	for _, name := range []string{"python3", "bash"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skip(name + " is unavailable")
		}
	}
	lifetime, cancel := context.WithCancel(context.Background())
	session, starts = &guestSession{}, &atomic.Int32{}
	t.Cleanup(func() { session.close(); cancel() })
	command := func(ctx context.Context) *exec.Cmd {
		starts.Add(1)
		return exec.CommandContext(ctx, "python3", "-I", "-u", "-c", guestSessionServer)
	}
	return func(ctx context.Context, script string) (guestSessionReply, bool, error) {
		return session.run(ctx, lifetime, command, script)
	}, session, starts
}

func TestGuestSessionRunsScriptsOverOneConnection(t *testing.T) {
	run, _, starts := localGuestSession(t)
	ctx := context.Background()
	reply, sent, err := run(ctx, "printf 'out %s' \"$0\"; printf problem >&2; exit 7")
	if err != nil || !sent || reply.code != 7 || string(reply.stdout) != "out /bin/bash" || string(reply.stderr) != "problem" {
		t.Fatalf("reply %+v sent=%v err=%v", reply, sent, err)
	}
	// Each script is a separate shell: no state passes between them.
	if reply, _, err = run(ctx, "cd /tmp; export LEFT=1"); err != nil || reply.code != 0 {
		t.Fatal(reply, err)
	}
	if reply, _, err = run(ctx, "printf '%s' \"${LEFT:-unset}\"; test \"$PWD\" != /tmp"); err != nil || reply.code != 0 || string(reply.stdout) != "unset" {
		t.Fatalf("state leaked between scripts: %+v %v", reply, err)
	}
	// stdin is empty, as on a connection of its own.
	if reply, _, err = run(ctx, "cat; printf done"); err != nil || string(reply.stdout) != "done" {
		t.Fatalf("stdin was not empty: %+v %v", reply, err)
	}
	if reply, _, err = run(ctx, "printf '多字节 ✓\\n\\0binary'"); err != nil || string(reply.stdout) != "多字节 ✓\n\x00binary" {
		t.Fatalf("output changed in transit: %q %v", reply.stdout, err)
	}
	if starts.Load() != 1 {
		t.Fatalf("%d connections were opened for consecutive scripts", starts.Load())
	}
}

func TestGuestSessionSlowScriptDoesNotDelayOthers(t *testing.T) {
	run, _, starts := localGuestSession(t)
	slow := make(chan error, 1)
	go func() {
		reply, _, err := run(context.Background(), "sleep 1; printf slow")
		if err == nil && string(reply.stdout) != "slow" {
			err = errors.New("slow script lost its output")
		}
		slow <- err
	}()
	time.Sleep(100 * time.Millisecond)
	started := time.Now()
	var group sync.WaitGroup
	for i := 0; i < 8; i++ {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			reply, _, err := run(context.Background(), "printf "+strconv.Itoa(i))
			if err != nil || string(reply.stdout) != strconv.Itoa(i) {
				t.Errorf("script %d: %+v %v", i, reply, err)
			}
		}(i)
	}
	group.Wait()
	if time.Since(started) > 700*time.Millisecond {
		t.Fatal("quick scripts waited for the slow one")
	}
	if err := <-slow; err != nil {
		t.Fatal(err)
	}
	if starts.Load() != 1 {
		t.Fatal("concurrent scripts opened more connections")
	}
}

func TestGuestSessionCancellationStopsTheScript(t *testing.T) {
	run, _, _ := localGuestSession(t)
	file := filepath.Join(t.TempDir(), "pid")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := run(ctx, "sleep 60 & echo $! > "+file+"; wait")
		done <- err
	}()
	var pid int
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if data, err := os.ReadFile(file); err == nil && len(data) > 1 {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
			break
		}
	}
	if pid == 0 {
		t.Fatal("script did not start")
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if syscall.Kill(pid, 0) != nil {
			break
		}
	}
	if syscall.Kill(pid, 0) == nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatal("a cancelled script kept its background process")
	}
	// The connection stays usable.
	if reply, _, err := run(context.Background(), "printf ok"); err != nil || string(reply.stdout) != "ok" {
		t.Fatal(reply, err)
	}
}

func TestGuestSessionBoundsOutput(t *testing.T) {
	run, _, _ := localGuestSession(t)
	started := time.Now()
	reply, _, err := run(context.Background(), "yes | head -c 100000000; yes x >&2 | true; head -c 100000 /dev/zero >&2")
	if err != nil || len(reply.stdout) <= 64<<10 || len(reply.stdout) > 64<<10+256 || len(reply.stderr) > 4096 || reply.code == 0 {
		t.Fatalf("stdout=%d stderr=%d code=%d err=%v", len(reply.stdout), len(reply.stderr), reply.code, err)
	}
	if time.Since(started) > 5*time.Second {
		t.Fatal("an oversized answer was not stopped")
	}
}

func TestGuestSessionFallsBackOnlyBeforeTheScriptIsSent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the control session serves Linux guests")
	}
	session, starts := &guestSession{}, 0
	missing := func(ctx context.Context) *exec.Cmd {
		starts++
		return exec.CommandContext(ctx, filepath.Join(t.TempDir(), "missing"))
	}
	for i := 0; i < 3; i++ {
		if _, sent, err := session.run(context.Background(), context.Background(), missing, "true"); sent || err == nil {
			t.Fatal("an unavailable session claimed to run the script")
		}
	}
	// It is not started again for every poll.
	if starts != 1 {
		t.Fatalf("an unavailable session was started %d times", starts)
	}

	run, session, opened := localGuestSession(t)
	done := make(chan error, 1)
	go func() {
		_, sent, err := run(context.Background(), "sleep 30")
		if !sent {
			err = errors.New("a script cut off by the connection was reported as unsent")
		}
		done <- err
	}()
	time.Sleep(200 * time.Millisecond)
	session.close()
	if err := <-done; !errors.Is(err, errGuestSessionEnded) {
		t.Fatal(err)
	}
	if reply, sent, err := run(context.Background(), "printf again"); err != nil || !sent || string(reply.stdout) != "again" || opened.Load() != 2 {
		t.Fatalf("a closed session did not reopen: %+v %v %v %d", reply, sent, err, opened.Load())
	}
}

func TestGuestSessionToleratesLoginShellOutputAndRejectsOtherServers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the control session serves Linux guests")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 is unavailable")
	}
	noisy := func(ctx context.Context) *exec.Cmd {
		return exec.CommandContext(ctx, "bash", "-c", "echo 'Welcome to the guest'; exec python3 -I -u -c \"$0\"", guestSessionServer)
	}
	session := &guestSession{}
	defer session.close()
	if reply, sent, err := session.run(context.Background(), context.Background(), noisy, "printf ok"); err != nil || !sent || string(reply.stdout) != "ok" {
		t.Fatalf("%+v %v %v", reply, sent, err)
	}
	other := &guestSession{}
	defer other.close()
	silent := func(ctx context.Context) *exec.Cmd {
		return exec.CommandContext(ctx, "bash", "-c", "echo not-a-session; sleep 30")
	}
	started := time.Now()
	if _, sent, err := other.run(context.Background(), context.Background(), silent, "true"); sent || err == nil {
		t.Fatal("an unknown server was used")
	}
	if time.Since(started) > guestSessionStartup+2*time.Second {
		t.Fatal("startup was not bounded")
	}
}
