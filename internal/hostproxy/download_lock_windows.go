package hostproxy

import (
	"context"
	"errors"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

func withDownloadLock(ctx context.Context, dir string, fn func() error) error {
	// Keep the existing lock filename and byte range for old app instances.
	file, err := os.OpenFile(dir+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	var overlapped windows.Overlapped
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err = windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped)
		if err == nil {
			defer windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &overlapped)
			return fn()
		}
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}
