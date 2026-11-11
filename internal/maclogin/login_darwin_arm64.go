//go:build darwin && arm64 && cgo && !no_vz

// Package maclogin presents only the Apple login screen of the bound trial VM.
// The owner of the VZ instance creates its view on the AppKit main thread.
package maclogin

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework AppKit -framework Virtualization
#include "bridge.h"
*/
import "C"

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"al.essio.dev/pkg/shellescape"
	"time"
	"unsafe"

	apple "github.com/Code-Hex/vz/v3"
	driver "github.com/lima-vm/lima/v2/pkg/driver/vz"
	"github.com/sylvinus/agent-vm/internal/guestos"
)

type objectReference interface{ CageViewObject() unsafe.Pointer }

func init() {
	if _, built := any((*apple.VirtualMachine)(nil)).(objectReference); !built {
		return
	}
	if len(os.Args) > 1 && (os.Args[1] == "hostagent" || os.Args[1] == "--debug") {
		runtime.LockOSThread()
	}
	driver.OwnsOffscreenView = func(dir string) bool {
		config := os.Getenv("CLAUDE_CAGE_CONFIG")
		if !filepath.IsAbs(config) || !sameGuestDirectory(dir, filepath.Join(filepath.Dir(config), "state", "lima", "mac")) {
			return false
		}
		profile, err := guestos.LoadMacOSProfile(filepath.Dir(config))
		return err == nil && profile != nil
	}
	driver.OffscreenView = run
}

type request = guestos.MacLoginRequest
type reply = guestos.MacLoginReply

type loginSession struct {
	desktop                  bool
	profile                  string
	token                    string
	expires                  time.Time
	screenID                 string
	setup                    bool
	maintenance              string
	maintenanceFrame         string
	maintenanceMachineID     string
	maintenanceAdministrator string
	passwordSelection        passwordSelection
}

func (s *loginSession) currentScreen() string {
	if s.maintenance != "" {
		surface, err := s.maintenanceProbe(false)
		if err != nil {
			return ""
		}
		return "maintenance-" + surface.Session
	}
	// Check via the separate provisioning account. Guest development files
	// and the standard user's shell initialization cannot grant this view.
	bound, err := guestos.LoadMacOSProfile(s.profile)
	if err != nil || bound == nil {
		return ""
	}
	if s.setup {
		if bound.SetupID == "" {
			return ""
		}
		return "setup-" + bound.SetupID
	}
	user, err := guestos.MacOSProvisionUser(s.profile)
	if err != nil {
		return ""
	}
	args, err := guestos.MacOSSSHArgs(s.profile, user)
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	script := `set -eu
/usr/bin/pgrep -x loginwindow >/dev/null
owner=$(/usr/bin/stat -f '%u %Su' /dev/console)
if [ "$owner" = "0 root" ]; then /usr/bin/printf 'system-login'; exit 0; fi
uid=$(/usr/bin/id -u ` + shellescape.Quote(bound.User) + `)
/usr/bin/printf '%s %s\n' "$owner" "$uid"
/usr/sbin/ioreg -a -d1 -c IORegistryEntry -k IOConsoleUsers -l | /usr/bin/plutil -extract IOConsoleUsers xml1 -o - -`
	cmd := exec.CommandContext(ctx, "/usr/bin/ssh", append(args, "-T", "cage-macos", "--", "exec /bin/sh -c "+shellescape.Quote(script))...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LC_ALL=C"}
	reader, err := cmd.StdoutPipe()
	if err != nil || cmd.Start() != nil {
		return ""
	}
	b, readErr := io.ReadAll(io.LimitReader(reader, (32<<10)+1))
	if readErr != nil || len(b) > 32<<10 {
		cancel()
	}
	if err = cmd.Wait(); err != nil || readErr != nil || len(b) > 32<<10 {
		return ""
	}
	return permittedLoginScreen(bytes.TrimSpace(b), bound.User)
}

func (s *loginSession) close() {
	if s.desktop {
		C.cageDesktopClose()
	} else if s.token != "" {
		C.cageLoginEnable(false)
	}
	s.desktop = false
	s.token = ""
	s.screenID = ""
	s.setup = false
	s.maintenance = ""
	s.maintenanceFrame = ""
	s.maintenanceMachineID = ""
	s.maintenanceAdministrator = ""
	s.passwordSelection = passwordSelection{}
}

func (s *loginSession) handle(in request) reply {
	if strings.HasPrefix(in.Action, "desktop-") {
		return s.handleDesktop(in)
	}
	if s.desktop {
		if in.Action != "open" {
			return reply{Closed: true}
		}
		s.close()
	}
	if in.Maintenance != "" || (s.maintenance != "" && in.Action != "open") {
		return s.handleMaintenance(in)
	}
	if in.Action == "finish" && in.Setup {
		bound, err := guestos.LoadMacOSProfile(s.profile)
		if err != nil || bound == nil || bound.SetupID == "" {
			return reply{Closed: true}
		}
		s.close()
		if err = guestos.FinishMacOSSetup(s.profile, bound.SetupID); err != nil {
			return reply{Error: "未能保存首次设置，请重试"}
		}
		return reply{Closed: true}
	}
	if in.Action == "open" {
		s.close()
		s.setup = in.Setup
		s.screenID = s.currentScreen()
		if s.screenID == "" {
			return reply{Error: "客机已解锁，或尚未进入系统登录画面。"}
		}
		s.token = rand.Text()
		s.expires = time.Now().Add(2 * time.Minute)
		if s.setup {
			s.expires = time.Now().Add(15 * time.Minute)
		}
		C.cageLoginEnable(true)
		return reply{Session: s.token}
	}
	if s.token == "" || s.setup != in.Setup || subtle.ConstantTimeCompare([]byte(in.Session), []byte(s.token)) != 1 {
		return reply{Closed: true}
	}
	if time.Now().After(s.expires) || in.Action == "close" || s.currentScreen() != s.screenID {
		s.close()
		return reply{Closed: true}
	}
	switch in.Action {
	case "fill-password":
		if err := s.fillSetupPassword(in.Password); err != nil {
			return reply{Error: err.Error()}
		}
	case "frame":
		var n C.int
		p := C.cageLoginImage(&n)
		if p == nil {
			return reply{Error: "尚未收到 Apple 登录画面"}
		}
		defer C.free(p)
		if s.currentScreen() != s.screenID {
			s.close()
			return reply{Closed: true}
		}
		return reply{Image: C.GoBytes(p, n), MIME: "image/jpeg"}
	case "click":
		s.passwordSelection = passwordSelection{}
		if in.X < 0 || in.Y < 0 || in.X >= 1024 || in.Y >= 640 || !bool(C.cageLoginClick(C.int(in.X), C.int(in.Y))) {
			return reply{Error: "无效的登录位置"}
		}
		if s.setup {
			s.selectPasswordPosition(in.X, in.Y)
		}
	case "key":
		s.passwordSelection = passwordSelection{}
		// ANSI printable keys, Return, Tab, Escape and Backspace. No global shortcuts.
		setupNavigation := s.setup && (in.Code >= 123 && in.Code <= 126 || in.Code == 116 || in.Code == 121)
		if in.Code < 0 || (in.Code > 53 && !setupNavigation) || !bool(C.cageLoginKey(C.int(in.Code), C.bool(in.Shift))) {
			return reply{Error: "不支持此登录按键"}
		}
	case "scroll":
		s.passwordSelection = passwordSelection{}
		if !s.setup || in.X < 0 || in.Y < 0 || in.X >= 1024 || in.Y >= 640 ||
			in.DeltaX < -512 || in.DeltaX > 512 || in.DeltaY < -512 || in.DeltaY > 512 ||
			!bool(C.cageLoginScroll(C.int(in.X), C.int(in.Y), C.int(in.DeltaX), C.int(in.DeltaY))) {
			return reply{Error: "无效的首次设置滚动操作"}
		}
	default:
		return reply{Error: "未知的登录操作"}
	}
	return reply{}
}

// Desktop access is a separate capability from the limited login controls.
// The controller renews this short lease only while its trusted local UI lives.
func (s *loginSession) handleDesktop(in request) reply {
	result := reply{DesktopVersion: 1}
	if in.Setup || in.Maintenance != "" {
		result.Error = "无效的桌面操作"
		return result
	}
	if in.Action == "desktop-open" {
		bound, err := guestos.LoadMacOSProfile(s.profile)
		if err != nil || bound == nil || bound.SetupID != "" {
			result.Error = "请先完成虚拟 Mac 首次设置"
			return result
		}
		s.close()
		if !bool(C.cageDesktopOpen()) {
			result.Error = "虚拟 Mac 尚未就绪"
			return result
		}
		s.desktop = true
		s.token = rand.Text()
		s.expires = time.Now().Add(45 * time.Second)
		result.Session = s.token
		return result
	}
	if !s.desktop || s.token == "" || subtle.ConstantTimeCompare([]byte(in.Session), []byte(s.token)) != 1 {
		result.Closed = true
		return result
	}
	if in.Action == "desktop-close" || time.Now().After(s.expires) || !bool(C.cageDesktopVisible()) {
		s.close()
		result.Closed = true
		return result
	}
	if in.Action != "desktop-status" {
		result.Error = "无效的桌面操作"
		return result
	}
	s.expires = time.Now().Add(45 * time.Second)
	return result
}

func run(ctx context.Context, machine *apple.VirtualMachine, dir string) error {
	if !driver.OwnsOffscreenView(dir) {
		return errors.New("invalid local macOS login view")
	}
	socket := filepath.Join(dir, "cage-login.sock")
	if fi, err := os.Lstat(socket); err == nil {
		if fi.Mode()&os.ModeSocket == 0 {
			return errors.New("unexpected login socket path")
		}
		existing, err := net.DialTimeout("unix", socket, 300*time.Millisecond)
		if err == nil {
			existing.Close()
			return errors.New("this instance already has a login view")
		}
		if err = os.Remove(socket); err != nil {
			return err
		}
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		return err
	}
	defer ln.Close()
	if err = os.Chmod(socket, 0600); err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	s := loginSession{profile: filepath.Dir(os.Getenv("CLAUDE_CAGE_CONFIG"))}
	go func() {
		for {
			_ = ln.SetDeadline(time.Now().Add(time.Second))
			conn, err := ln.Accept()
			if err != nil {
				if n, ok := err.(net.Error); ok && n.Timeout() && runCtx.Err() == nil {
					if s.token != "" && (time.Now().After(s.expires) || (!s.desktop && s.currentScreen() != s.screenID) || (s.desktop && !bool(C.cageDesktopVisible()))) {
						s.close()
					}
					continue
				}
				return
			}
			_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
			var input request
			if json.NewDecoder(io.LimitReader(conn, 4096)).Decode(&input) == nil {
				if input.Maintenance != "" {
					_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
				}
				result := s.handle(input)
				if err := json.NewEncoder(conn).Encode(result); err != nil && result.Session != "" {
					s.close()
				}
			}
			_ = conn.Close()
		}
	}()
	go func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		running := false
		for {
			select {
			case <-runCtx.Done():
				C.cageLoginStop()
				return
			case <-ticker.C:
				state := machine.State()
				if state == apple.VirtualMachineStateRunning {
					running = true
				}
				if running && (state == apple.VirtualMachineStateStopped || state == apple.VirtualMachineStateError) {
					C.cageLoginStop()
					return
				}
			}
		}
	}()
	ref, ok := any(machine).(objectReference)
	if !ok {
		return errors.New("this build has no macOS login view")
	}
	C.cageLoginLoop(ref.CageViewObject())
	runtime.KeepAlive(machine)
	return nil
}
