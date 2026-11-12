//go:build windows

package guestos

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// This exercises the shipping helper and PowerShell ready/DPAPI protocol using
// an ordinary token. Elevated CI runners must not mask the owner's pipe ACL.
func TestWindowsHostEnrollmentOrdinaryUser(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	manager := os.Getenv("CAGE_TEST_WINDOWS_ENROLLMENT_MANAGER")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if manager == "" {
		manager = filepath.Join(t.TempDir(), "Cage.HyperVManager.exe")
		build := exec.CommandContext(ctx, "go", "build", "-o", manager, "./cmd/cage-hyperv-manager")
		build.Dir = root
		if output, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build enrollment helper: %v\n%s", err, output)
		}
	}
	runWindowsOrdinaryUserPowerShell(t, filepath.Join(root, "scripts", "guest-os", "windows-host-enrollment-smoke.ps1"), "-ManagerExecutable", manager)
}

func TestWindowsUserToolACLOrdinaryUser(t *testing.T) {
	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "guest-os", "windows-user-tools-acl.tests.ps1"))
	if err != nil {
		t.Fatal(err)
	}
	runWindowsOrdinaryUserPowerShell(t, script)
}

func runWindowsOrdinaryUserPowerShell(t *testing.T, script string, arguments ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	shell := filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	flags := append([]string{"-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-File", script}, arguments...)
	cmd := exec.CommandContext(ctx, shell, flags...)
	if windows.GetCurrentProcessToken().IsElevated() {
		var token windows.Token
		if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_DUPLICATE|windows.TOKEN_QUERY|windows.TOKEN_ASSIGN_PRIMARY|windows.TOKEN_ADJUST_DEFAULT, &token); err != nil {
			t.Fatal(err)
		}
		defer token.Close()
		ordinary, err := token.GetLinkedToken()
		if err != nil {
			// Some disposable CI runners have UAC disabled and no linked token.
			// LUA_TOKEN also filters administrator account groups which removing
			// privileges alone leaves elevated from TokenElevation's perspective.
			create := windows.NewLazySystemDLL("advapi32.dll").NewProc("CreateRestrictedToken")
			const disableMaxPrivilegeAndLUA = 0x1 | 0x4
			ok, _, callErr := create.Call(uintptr(token), disableMaxPrivilegeAndLUA, 0, 0, 0, 0, 0, 0, uintptr(unsafe.Pointer(&ordinary)))
			if ok == 0 {
				t.Fatal(callErr)
			}
			defer ordinary.Close()
			medium, err := windows.CreateWellKnownSid(windows.WinMediumLabelSid)
			if err != nil {
				t.Fatal(err)
			}
			label := windows.Tokenmandatorylabel{Label: windows.SIDAndAttributes{Sid: medium, Attributes: windows.SE_GROUP_INTEGRITY}}
			if err := windows.SetTokenInformation(ordinary, windows.TokenIntegrityLevel, (*byte)(unsafe.Pointer(&label)), label.Size()); err != nil {
				t.Fatal(err)
			}
			user, err := ordinary.GetTokenUser()
			if err != nil {
				t.Fatal(err)
			}
			security, err := windows.SecurityDescriptorFromString("D:(A;;GA;;;" + user.User.Sid.String() + ")(A;;GA;;;SY)(A;;GA;;;BA)")
			if err != nil {
				t.Fatal(err)
			}
			dacl, _, err := security.DACL()
			if err != nil {
				t.Fatal(err)
			}
			defaults := struct{ ACL *windows.ACL }{dacl}
			err = windows.SetTokenInformation(ordinary, windows.TokenDefaultDacl, (*byte)(unsafe.Pointer(&defaults)), uint32(unsafe.Sizeof(defaults)))
			runtime.KeepAlive(security)
			if err != nil {
				t.Fatal(err)
			}
		} else {
			defer ordinary.Close()
		}
		if ordinary.IsElevated() {
			t.Fatal("the native regression requires an unelevated token")
		}
		cmd.SysProcAttr = &syscall.SysProcAttr{Token: syscall.Token(ordinary)}
	}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ordinary-user PowerShell %s: %v\n%s", filepath.Base(script), err, output)
	}
}
