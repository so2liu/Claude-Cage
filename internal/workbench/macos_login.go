package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/sylvinus/agent-vm/internal/cage"
	"io"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/sylvinus/agent-vm/internal/guestos"
	"github.com/sylvinus/agent-vm/internal/host"
)

// Only installer-created bindings carry this identity. A damaged binding stays
// quarantined; the VM process independently requires a valid matching binding.
func (s *Server) macSetupPending() bool {
	if s.macosGuest == nil || s.macosGuest.SetupID == "" {
		return false
	}
	current, err := guestos.LoadMacOSProfile(s.profile)
	return err != nil || current == nil || current.SetupID != ""
}

func macOSSetupRoute(path string) bool {
	if strings.HasPrefix(path, "/api/agent/jobs/") {
		return true
	}
	if !strings.HasPrefix(path, "/api/") {
		return true
	}
	switch path {
	case "/api/account", "/api/appearance", "/api/session", "/api/launch", "/api/handoff", "/api/remote/manifest", "/api/status", "/api/start", "/api/stop", "/api/macos/first-setup", "/api/environment/initialization":
		return true
	}
	return strings.HasPrefix(path, "/api/account/") || strings.HasPrefix(path, "/api/diagnostics")
}

type macSetupPermissions struct {
	Screen            bool   `json:"screen"`
	Input             bool   `json:"input"`
	Projects          bool   `json:"projects"`
	Error             string `json:"error,omitempty"`
	ManagedCredential bool   `json:"managed_credential,omitempty"`
}

type macLoginLease struct {
	request     guestos.MacLoginRequest
	stopAccount func() bool
	stopService func() bool
}

func (l *macLoginLease) stop() {
	if l == nil {
		return
	}
	l.stopAccount()
	l.stopService()
}

func (s *Server) revokeMacLogin(session string) {
	s.mu.Lock()
	lease := s.macLoginLease
	if lease == nil || lease.request.Session != session {
		s.mu.Unlock()
		return
	}
	s.macLoginLease = nil
	s.mu.Unlock()
	lease.stop()
	connection, err := net.DialTimeout("unix", filepath.Join(s.instanceDir(), "cage-login.sock"), 2*time.Second)
	if err != nil {
		return
	}
	defer connection.Close()
	_ = connection.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_ = json.NewEncoder(connection).Encode(lease.request)
}

func (s *Server) trackMacLogin(epoch context.Context, input guestos.MacLoginRequest, result guestos.MacLoginReply) {
	if (input.Action == "open" || input.Action == "desktop-open") && result.Session != "" {
		lifetime := s.lifetimeContext()
		closeAction := "close"
		if input.Action == "desktop-open" {
			closeAction = "desktop-close"
		}
		lease := &macLoginLease{request: guestos.MacLoginRequest{Action: closeAction, Session: result.Session, Setup: input.Setup, Maintenance: input.Maintenance}}
		s.mu.Lock()
		previous := s.macLoginLease
		s.macLoginLease = lease
		lease.stopAccount = context.AfterFunc(epoch, func() { s.revokeMacLogin(result.Session) })
		lease.stopService = context.AfterFunc(lifetime, func() { s.revokeMacLogin(result.Session) })
		s.mu.Unlock()
		previous.stop()
		return
	}
	if input.Action == "close" || input.Action == "desktop-close" || result.Closed {
		s.mu.Lock()
		lease := s.macLoginLease
		if lease != nil && lease.request.Session == input.Session {
			s.macLoginLease = nil
		} else {
			lease = nil
		}
		s.mu.Unlock()
		lease.stop()
	}
}

func (s *Server) macSetupPermissions(ctx context.Context) (macSetupPermissions, error) {
	var status macSetupPermissions
	data, err := s.macOSWindowCall(ctx, macOSWindowRequest{Action: "setup-status"})
	if err != nil {
		return status, err
	}
	if json.Unmarshal(data, &status) != nil || status.Error != "" {
		return status, errors.New("请在首次设置画面中登录客机账户")
	}
	s.mu.Lock()
	status.Projects = !s.macSetupProjectsChecked.IsZero() && time.Since(s.macSetupProjectsChecked) < 2*time.Minute
	probeDue := time.Since(s.macSetupProjectsAttempt) > 15*time.Second
	s.mu.Unlock()
	if status.Screen && status.Input && !status.Projects && probeDue && s.op.TryLock() {
		s.mu.Lock()
		s.macSetupProjectsAttempt = time.Now()
		s.mu.Unlock()
		_ = s.checkMacOSSetupProjects(ctx)
		s.op.Unlock()
		s.mu.Lock()
		status.Projects = !s.macSetupProjectsChecked.IsZero()
		s.mu.Unlock()
	}
	if install, err := guestos.LoadInstallation(s.profile); err == nil && install != nil {
		password, err := s.account.savedMacGuestPassword(ctx, install.ID)
		status.ManagedCredential = err == nil && password != ""
	}
	return status, nil
}

func (s *Server) macOSLoginWindow(w http.ResponseWriter, r *http.Request) {
	if s.macosGuest == nil || !s.hasTrustedView(r) {
		http.NotFound(w, r)
		return
	}
	var input guestos.MacLoginRequest
	if !readJSON(w, r, &input) {
		return
	}
	input.Setup = r.URL.Path == "/api/macos/first-setup"
	desktop := r.URL.Path == "/api/macos/desktop"
	if desktop {
		switch input.Action {
		case "desktop-open", "desktop-close", "desktop-status":
		default:
			http.Error(w, "无效的桌面操作", http.StatusBadRequest)
			return
		}
		if s.macSetupPending() {
			http.Error(w, "请先完成首次设置", http.StatusConflict)
			return
		}
		input = guestos.MacLoginRequest{Action: input.Action, Session: input.Session}
	} else if r.URL.Path == "/api/macos/maintenance" {
		if !guestos.ValidMacMaintenancePurpose(input.Maintenance) {
			http.Error(w, "请选择需要恢复的客机系统权限", http.StatusBadRequest)
			return
		}
		switch input.Action {
		case "open", "close", "frame", "click", "key", "scroll", "select-all", "fill-password":
		default:
			http.Error(w, "维护画面只接受窗口查看与局部输入", http.StatusBadRequest)
			return
		}
	} else {
		input.Maintenance = ""
	}
	if input.Setup && !s.macSetupPending() {
		http.Error(w, "首次设置入口已关闭", http.StatusGone)
		return
	}
	// Passwords are supplied exclusively from encrypted storage, never from HTTP.
	input.Password = ""
	if input.Action == "show-password" {
		install, err := guestos.LoadInstallation(s.profile)
		bound, bindingErr := guestos.LoadMacOSProfile(s.profile)
		if err != nil || bindingErr != nil || install == nil || bound == nil || bound.MachineID != s.macosGuest.MachineID {
			http.Error(w, "客机身份已改变", 409)
			return
		}
		password, err := s.account.savedMacGuestPassword(r.Context(), install.ID)
		if err != nil || password == "" {
			http.Error(w, "客机凭据不可用，请核对安装记录与保险箱", 409)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		jsonReply(w, map[string]string{"password": password})
		return
	}
	if input.Setup {
		gate, err := s.gateClient()
		if err != nil {
			http.Error(w, "请先启动客机，确认首次设置期间外网已关闭", 409)
			return
		}
		check, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		state, err := gate.Status(check)
		cancel()
		if err != nil || state.Mode != cage.GateClosed {
			http.Error(w, "首次设置仅在客机断网时可用", 409)
			return
		}
		switch input.Action {
		case "fill-password":
			// The native runner fills macOS's own authorization dialog. The browser
			// never receives the stored credential; no submit key is sent.
			install, err := guestos.LoadInstallation(s.profile)
			if err != nil || install == nil || install.ID.String() != s.macosGuest.SetupID {
				http.Error(w, "首次设置身份已改变", 409)
				return
			}
			password, err := s.account.savedMacGuestPassword(r.Context(), install.ID)
			if err != nil || password == "" {
				http.Error(w, "客机凭据不可用，请核对安装记录与保险箱", 409)
				return
			}
			input.Password = password
		case "permissions", "verify-projects", "finish":
			perm, err := s.macSetupPermissions(r.Context())
			if err != nil {
				http.Error(w, err.Error(), 409)
				return
			}
			if input.Action == "permissions" {
				jsonReply(w, perm)
				return
			}
			if input.Action == "finish" && (!perm.Screen || !perm.Input) {
				http.Error(w, "请先完成窗口录制、辅助功能和文件系统扩展三项授权", 409)
				return
			}
			if !s.op.TryLock() {
				http.Error(w, "正在处理其他客机操作，请重试", 409)
				return
			}
			defer s.op.Unlock()
			if err = s.checkMacOSSetupProjects(r.Context()); err != nil {
				http.Error(w, err.Error(), 409)
				return
			}
			if input.Action == "verify-projects" {
				perm.Projects = true
				jsonReply(w, perm)
				return
			}
			// The cleanup executable is fixed, root-owned and verifies the temporary
			// setup administrator before retiring it. Its output contains no credentials.
			ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
			defer cancel()
			cmd := s.macOSSetupAdminCommand(ctx)
			if err = cmd.Run(); err != nil {
				http.Error(w, "临时设置账户未关闭，请重试完成设置", 409)
				return
			}
		case "screen", "input", "projects":
			s.macOSWindowRPC(w, r, macOSWindowRequest{Action: "setup-" + input.Action})
			return
		case "open", "close", "frame", "click", "key", "scroll":
		default:
			http.Error(w, "无效的首次设置操作", 400)
			return
		}
	} else if !desktop && input.Maintenance == "" {
		switch input.Action {
		case "open", "close", "frame", "click", "key", "scroll":
		default:
			http.Error(w, "无效的登录操作", 400)
			return
		}
	}
	if len(input.Session) > 80 {
		http.Error(w, "无效的登录会话", 400)
		return
	}
	timeout := 8 * time.Second
	if input.Maintenance != "" {
		timeout = 15 * time.Second
	}
	epoch, _ := s.account.accessFor(true)
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	var dial net.Dialer
	conn, err := dial.DialContext(ctx, "unix", filepath.Join(s.instanceDir(), "cage-login.sock"))
	if err != nil {
		http.Error(w, "这台客机尚未启动带登录控件的运行器", 409)
		return
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	var result guestos.MacLoginReply
	if json.NewEncoder(conn).Encode(input) != nil || json.NewDecoder(io.LimitReader(conn, 6<<20)).Decode(&result) != nil {
		http.Error(w, "客机登录界面无法响应", 503)
		return
	}
	if input.Maintenance != "" && result.MaintenanceVersion != 1 {
		result = guestos.MacLoginReply{Closed: true, Recovery: "environment-restart", Error: "虚拟 Mac 运行器需要重新加载。请保存工作，在环境设置中停止并重新启动虚拟 Mac，再打开授权设置。"}
	}
	if desktop && result.DesktopVersion != 1 {
		result = guestos.MacLoginReply{Closed: true, Recovery: "environment-restart", Error: "虚拟 Mac 运行器需要更新。请保存工作，停止并重新启动虚拟 Mac，再打开桌面。"}
	}
	s.trackMacLogin(epoch, input, result)
	if (input.Action == "open" || input.Action == "desktop-open") && ctx.Err() != nil {
		s.revokeMacLogin(result.Session)
		return
	}
	if input.Setup && input.Action == "finish" && result.Error == "" && !s.macSetupPending() {
		s.restoreProjectShares()
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(result); err != nil && (input.Action == "open" || input.Action == "desktop-open") {
		s.revokeMacLogin(result.Session)
	}
}

func (s *Server) macOSSetupAdminCommand(ctx context.Context) *exec.Cmd {
	user, err := guestos.MacOSProvisionUser(s.profile)
	if err != nil {
		return &exec.Cmd{Err: err}
	}
	args, err := guestos.MacOSSSHArgs(s.profile, user)
	if err != nil {
		return &exec.Cmd{Err: err}
	}
	cmd := exec.CommandContext(ctx, sshExecutable(), append(args, "-T", "cage-macos", "--", "exec /usr/bin/sudo -S -p '' /usr/local/libexec/cage-finish-initial-setup < /Users/"+user+"/password")...)
	cmd.Env = sshEnvironment()
	host.HideCommandWindow(cmd)
	return cmd
}
