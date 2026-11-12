package hostproxy

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDownloadLockCancellationAndReuse(t *testing.T) {
	dir := t.TempDir()
	held, release := make(chan struct{}), make(chan struct{})
	owner := make(chan error, 1)
	go func() {
		owner <- withDownloadLock(t.Context(), dir, func() error { close(held); <-release; return nil })
	}()
	<-held
	defer close(release)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := withDownloadLock(ctx, dir, func() error { t.Error("entered locked cache"); return nil })
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("waiting lock ignored cancellation: %v", err)
	}
	// Unblock the owner without double closing the deferred release channel.
	release <- struct{}{}
	if err := <-owner; err != nil {
		t.Fatal(err)
	}
	if err := withDownloadLock(t.Context(), dir, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
}
