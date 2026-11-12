package workbench

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sylvinus/agent-vm/internal/diagnostics"

	"al.essio.dev/pkg/shellescape"
)

func macOSProjectPath(user, hostDirectory string) string {
	digest := sha256.Sum256([]byte(hostDirectory))
	return path.Join("/Users", user, "Cage", "Projects", fmt.Sprintf("%x", digest[:12]))
}

// A mount which is ready in FSKit can still be unreadable by the guest app.
// Keep the live restricted connection while macOS waits for the user's folder
// approval; do not accept an APFS directory or a stale mount as this project.
var errMacOSProjectApproval = &projectShareError{Recovery: "project-approval", Message: "请在 Cage 显示的客机授权窗口中允许此项目，然后验证授权。"}

const macProjectAgent = "/Applications/CageProjectAccess.app/Contents/MacOS/CageProjectAccess"
const macProjectLoginCheck = `test "$(/usr/bin/stat -f %u /dev/console)" = "$(/usr/bin/id -u)" && /bin/launchctl print "gui/$(/usr/bin/id -u)" >/dev/null 2>&1`

func macOSProjectPrepareError(err error) error {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		switch exit.ExitCode() {
		case 73:
			return &projectShareError{Recovery: "guest-login", Message: "请先登录虚拟 Mac 的客机账户。登录完成后，Cage 会自动连接项目。"}
		case 74, 79:
			return &projectShareError{Recovery: "environment-update", Message: "客机项目共享组件尚未就绪。请同步环境组件，完成后重新连接项目。"}
		case 76:
			return &projectShareError{Recovery: "environment-restart", Message: "此项目还保留着旧的客机挂载。请保存工作，在环境设置中停止并重新启动虚拟 Mac，再连接项目。"}
		case 77, 78:
			return &projectShareError{Recovery: "project-directory", Message: "客机的项目挂载位置包含文件、链接或权限异常。请先保留其中的文件，再通过诊断记录检查该位置。"}
		}
	}
	return &projectShareError{Recovery: "project-retry", Message: "工作台暂时无法连接客机项目服务。连接恢复后会自动重试，也可以重新连接项目。"}
}

func (s *Server) ensureMacOSProjectShare(hostDir string) error {
	ctx, cancel := context.WithTimeout(s.operationContext(), 18*time.Second)
	defer cancel()
	guestDir := macOSProjectPath(s.macosGuest.User, hostDir)
	s.mu.Lock()
	share := s.projectShares[hostDir]
	s.mu.Unlock()
	if share != nil {
		select {
		case <-share.done:
			share.close()
			share = nil
		default:
		}
	}
	if share == nil {
		var err error
		share, err = s.startMacOSProjectShare(ctx, hostDir, guestDir)
		if err != nil {
			return err
		}
		s.mu.Lock()
		if s.projectShares == nil {
			s.projectShares = make(map[string]*projectShare)
		}
		s.projectShares[hostDir] = share
		s.mu.Unlock()
	}
	err := s.probeMacOSProject(ctx, hostDir, guestDir, share)
	if err == nil && share.detach == nil {
		// FSKit assigns an independent volume UUID after mount. Pin the volume
		// only after the signed-in app returned this host directory's challenge.
		share.detach, err = s.macOSMountCleanup(ctx, guestDir)
	}
	if errors.Is(err, errMacOSProjectApproval) {
		s.beginMacOSProjectApproval(hostDir, share)
	} else if err == nil {
		s.mu.Lock()
		share.approval = ""
		if share.approvalTimer != nil {
			share.approvalTimer.Stop()
			share.approvalTimer = nil
		}
		s.mu.Unlock()
	}
	if err != nil && !errors.Is(err, errMacOSProjectApproval) {
		share.close()
		s.mu.Lock()
		delete(s.projectShares, hostDir)
		s.mu.Unlock()
	}
	return err
}

func (s *Server) startMacOSProjectShare(ctx context.Context, hostDir, guestDir string) (*projectShare, error) {
	folder := shellescape.Quote(guestDir)

	prepare := `set -e; ` + macProjectLoginCheck + ` || exit 73
		test -x /usr/local/libexec/cage-sshfs && test -x ` + macProjectAgent + ` || exit 74
		p=` + folder + `; if /sbin/mount | /usr/bin/grep -Fq " on $p ("; then exit 76; fi; for d in "$HOME/Cage" "$HOME/Cage/Projects" "$p"; do
		test ! -L "$d" || exit 77; if test ! -e "$d"; then /bin/mkdir -m 700 "$d" || exit 78; fi; test -d "$d" && test -O "$d" || exit 77
	done; entries=$(/bin/ls -A "$p") || exit 78; test -z "$entries" || exit 77
	/usr/bin/open -gj -a /Applications/CageProjectAccess.app || exit 79
	if /usr/local/libexec/cage-sshfs --help 2>/dev/null | /usr/bin/grep -Fq 'cage_openwith='; then
		printf '\nCAGE_SSHFS_OPENWITH\n'
	fi`
	capabilities, err := s.guestControl(ctx, prepare, "macos-project-prepare")
	if err != nil {
		return nil, macOSProjectPrepareError(err)
	}
	command := "exec /usr/local/libexec/cage-sshfs " + shellescape.Quote(":"+filepath.ToSlash(hostDir)) + " " + folder +
		" -f -o slave,backend=fskit,nodev,nosuid,dir_cache=no"
	if strings.Contains("\n"+string(capabilities)+"\n", "\nCAGE_SSHFS_OPENWITH\n") {
		metadata := path.Join("/Users", s.macosGuest.User, "Library/Caches/dev.cage.GuestClaudeWindow/openwith.data")
		command += " -o " + shellescape.Quote("cage_openwith="+metadata)
	}
	cmd := s.sshCommand(s.lifetimeContext(), "-T", s.sshTarget(), "--", command)
	stderr := &fileControlStderr{}
	cmd.Stderr = stderr
	conn, err := startProjectTransport(cmd, hostDir)
	if err != nil {
		return nil, err
	}
	conn.stderr = stderr
	return conn, nil
}

func (s *Server) macOSMountCleanup(ctx context.Context, guestDir string) (func(), error) {
	query := "/sbin/mount | /usr/bin/grep -F -- " + shellescape.Quote(" on "+guestDir+" (")
	out, err := s.guest(ctx, query)
	line := strings.TrimSpace(string(out))
	device, options, ok := strings.Cut(line, " on "+guestDir+" (")
	id := strings.TrimPrefix(device, "macfuse://")
	if _, parseErr := uuid.Parse(id); err != nil || !ok || id == device || parseErr != nil || strings.Contains(line, "\n") ||
		!strings.Contains(options, "fskit") || !strings.Contains(options, "mounted by "+s.macosGuest.User+")") {
		return nil, errors.New("无法核对本次挂载的 macOS 卷标识")
	}
	detach := "if test \"$(" + query + ")\" = " + shellescape.Quote(line) + "; then /sbin/umount -f " + shellescape.Quote(guestDir) + "; fi"
	return func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, _ = s.guest(cleanup, detach)
	}, nil
}

func (s *Server) probeMacOSProject(ctx context.Context, hostDir, guestDir string, share *projectShare) error {
	probe, err := os.CreateTemp(hostDir, ".cage-mount-probe-*")
	if err != nil {
		return err
	}
	defer os.Remove(probe.Name())
	challenge := randomToken()
	_, writeErr := probe.WriteString(challenge)
	if err := errors.Join(writeErr, probe.Close()); err != nil {
		return err
	}
	cmd := shellescape.QuoteCommand([]string{macProjectAgent, "probe", guestDir, filepath.Base(probe.Name()), challenge})
	for {
		output, err := s.guest(ctx, cmd)
		if err == nil && strings.TrimSpace(string(output)) == "OK" {
			return nil
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 75 {
			return errMacOSProjectApproval
		}
		select {
		case <-ctx.Done():
			return &projectShareError{Recovery: "project-retry", Message: "客机项目助手尚未确认此文件夹可用。请重新连接项目，持续失败时可查看诊断记录。"}
		case <-share.done:
			if share.stderr != nil && len(share.stderr.data) > 0 {
				message := string(share.stderr.data)
				_ = diagnostics.New(s.profile).Record("warn", "macos-project-mount", message)
				if strings.Contains(strings.ToLower(message), "not enabled") {
					return &projectShareError{Recovery: "filesystem-permission", Message: "虚拟 Mac 的文件系统扩展尚未启用。请打开客机授权设置，按提示打开 macFUSE 和 macFUSE (local) 两个开关，Cage 会自动检查并重新连接项目。"}
				}
			}
			return &projectShareError{Recovery: "project-retry", Message: "客机文件共享连接已中断。Cage 会自动重试，持续失败时可查看诊断记录。"}
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// The existing lifecycle monitor retries only failed or ended project streams.
// A login check precedes retry so startup does not repeatedly open guest prompts.
func (s *Server) retryMacOSProjectShares(ctx context.Context) {
	s.mu.Lock()
	pending := make(map[string]bool)
	waitingForLogin := false
	waitingForFilesystem := false
	for _, failure := range s.projectErrors {
		if failure.Recovery == "guest-login" || failure.Recovery == "project-retry" || failure.Recovery == "filesystem-permission" {
			pending[failure.Path] = true
			waitingForLogin = waitingForLogin || failure.Recovery == "guest-login"
			waitingForFilesystem = waitingForFilesystem || failure.Recovery == "filesystem-permission"
		}
	}
	for path, share := range s.projectShares {
		select {
		case <-share.done:
			pending[path] = true
		default:
		}
	}
	retryAt := s.macProjectsRetryAt
	s.mu.Unlock()
	if len(pending) == 0 || (!waitingForLogin && time.Now().Before(retryAt)) {
		return
	}
	if _, err := s.guest(ctx, "set -e; "+macProjectLoginCheck+" || exit 73"); err != nil {
		for path := range pending {
			s.setProjectError(path, macOSProjectPrepareError(err))
		}
		return
	}
	s.mu.Lock()
	s.macProjectsRetryAt = time.Now().Add(15 * time.Second)
	s.mu.Unlock()
	if waitingForFilesystem && s.checkMacOSSetupProjects(s.lifetimeContext()) != nil {
		return
	}
	for path := range pending {
		_ = s.ensureProjectShare(path)
	}
}
