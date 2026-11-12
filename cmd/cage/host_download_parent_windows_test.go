package main

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestHostDownloadParentExitCancels(t *testing.T) {
	if os.Getenv("DOWNLOAD_PARENT_FIXTURE") == "1" {
		time.Sleep(30 * time.Second)
		return
	}
	process := exec.Command(os.Args[0], "-test.run=^TestHostDownloadParentExitCancels$")
	process.Env = append(os.Environ(), "DOWNLOAD_PARENT_FIXTURE=1")
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { process.Process.Kill(); process.Wait() }()
	ctx, cancel, err := hostDownloadParentContext(t.Context(), uint32(process.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	select {
	case <-ctx.Done():
		t.Fatal("live parent canceled download")
	default:
	}
	if err := process.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
		if ctx.Err() != context.Canceled {
			t.Fatal(ctx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("dead parent left download running")
	}
}
