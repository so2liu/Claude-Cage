//go:build !windows

package workbench

import (
	"context"
	"errors"
	"net"
	"os/exec"
	"testing"
)

func TestDesktopViewerConnectionReplacementEndsTheOneHoldingInput(t *testing.T) {
	cat, err := exec.LookPath("cat")
	if err != nil {
		t.Skip("needs cat as a stand-in relay")
	}
	open := func() (*nativeWindowConnection, error) {
		ctx, cancel := context.WithCancel(context.Background())
		return startWindowConnection(exec.CommandContext(ctx, cat), cancel)
	}
	first, err := open()
	if err != nil {
		t.Fatal(err)
	}
	connection := &desktopViewerConnection{current: first}
	if err := connection.request(map[string]any{"action": "frame"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := connection.replace(open); err != nil {
		t.Fatal(err)
	}
	// The guest releases a viewer's keys when its connection ends.
	if first.cmd.ProcessState == nil {
		t.Fatal("the connection holding input is still open")
	}
	if connection.current == first || connection.request(map[string]any{"action": "frame"}, nil) != nil {
		t.Fatal("the viewer has no working connection")
	}
	failure := errors.New("relay unavailable")
	if err := connection.replace(func() (*nativeWindowConnection, error) { return nil, failure }); err != failure {
		t.Fatalf("failed replacement hidden: %v", err)
	}
	if connection.request(map[string]any{"action": "frame"}, nil) != nil {
		t.Fatal("a failed replacement closed the working connection")
	}
	second := connection.current
	connection.Close()
	var late *nativeWindowConnection
	err = connection.replace(func() (*nativeWindowConnection, error) { late, err = open(); return late, err })
	if !errors.Is(err, net.ErrClosed) || second.cmd.ProcessState == nil || late == nil || late.cmd.ProcessState == nil {
		t.Fatalf("a closed viewer kept a guest connection: %v", err)
	}
}
