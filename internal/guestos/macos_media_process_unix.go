//go:build !windows

package guestos

import (
	"os/exec"
	"syscall"
)

func configureMediaCommand(cmd *exec.Cmd) {
	// Stop the shell and its curl/CLI-kit descendants together. Killing only
	// bash leaves downloads running after the UI releases the installation lock.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
