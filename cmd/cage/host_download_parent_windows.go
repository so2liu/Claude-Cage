package main

import (
	"context"
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// PowerShell can be terminated without delivering a console signal to its
// children. Retain a handle to its process so PID reuse cannot extend our life.
func hostDownloadContext(parent context.Context) (context.Context, context.CancelFunc, error) {
	return hostDownloadParentContext(parent, uint32(os.Getppid()))
}

func hostDownloadParentContext(parent context.Context, pid uint32) (context.Context, context.CancelFunc, error) {
	process, err := windows.OpenProcess(windows.SYNCHRONIZE, false, pid)
	if err != nil {
		return nil, nil, errors.New("下载启动进程已退出或无法访问")
	}
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer windows.CloseHandle(process)
		defer cancel()
		for ctx.Err() == nil {
			state, err := windows.WaitForSingleObject(process, 100)
			if err != nil || state != uint32(windows.WAIT_TIMEOUT) {
				return
			}
		}
	}()
	return ctx, func() { cancel(); <-done }, nil
}
