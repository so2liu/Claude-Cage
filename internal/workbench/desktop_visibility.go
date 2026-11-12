package workbench

import (
	"context"
	_ "embed"
	"strconv"
	"sync"
	"time"

	"github.com/sylvinus/agent-vm/internal/diagnostics"
)

//go:embed linux-desktop-visibility.py
var linuxDesktopVisibility string

// A controller owns all computer viewers and the paired phone's display lease.
// Serialize guest changes so a late hide completes before a new viewer resumes.
type displayViewDemand struct {
	mu           sync.Mutex
	op           sync.Mutex
	viewers      int
	idle         *time.Timer
	known, shown bool // guarded by op
}

func (d *displayViewDemand) apply(update func(bool) error) error {
	d.op.Lock()
	defer d.op.Unlock()
	d.mu.Lock()
	show := d.viewers > 0
	d.mu.Unlock()
	if d.known && d.shown == show {
		return nil
	}
	if err := update(show); err != nil {
		d.known = false
		return err
	}
	d.known, d.shown = true, show
	return nil
}

func (d *displayViewDemand) background(update func(bool) error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.viewers != 0 {
		return
	}
	if d.idle != nil {
		d.idle.Stop()
	}
	d.idle = time.AfterFunc(500*time.Millisecond, func() { _ = d.apply(update) })
}

func (d *displayViewDemand) acquire(update func(bool) error) (func(), error) {
	d.mu.Lock()
	d.viewers++
	if d.idle != nil {
		d.idle.Stop()
		d.idle = nil
	}
	d.mu.Unlock()
	var once sync.Once
	release := func() { once.Do(func() { d.mu.Lock(); d.viewers--; d.mu.Unlock(); d.background(update) }) }
	if err := d.apply(update); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

func (d *displayViewDemand) reset() {
	d.op.Lock()
	d.known = false
	d.op.Unlock()
}

func (s *Server) updateDesktopVisibility(show bool) error {
	// Linux uses a dedicated virtual X display. Native Windows/macOS capture
	// already follows viewer lifetime and retains its ordinary guest windows.
	if s.windowsGuest != nil || s.macosGuest != nil || !s.vmRunning() {
		return nil
	}
	ctx, cancel := context.WithTimeout(s.lifetimeContext(), 5*time.Second)
	defer cancel()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	script := "/usr/bin/python3 - " + strconv.FormatBool(show) + " <<'CAGE_DESKTOP_VISIBILITY'\n" + linuxDesktopVisibility + "\nCAGE_DESKTOP_VISIBILITY\n"
	_, err := s.guestQuick(ctx, script)
	if err != nil && ctx.Err() == nil {
		_ = diagnostics.New(s.profile).Record("warn", "desktop-visibility", "应用窗口显示状态未能同步："+err.Error())
	}
	return err
}

func (s *Server) acquireDesktopView() (func(), error) {
	if s.windowsGuest != nil || s.macosGuest != nil {
		return func() {}, nil
	}
	return s.desktopViews.acquire(s.changeDesktopVisibility)
}

func (s *Server) changeDesktopVisibility(show bool) error {
	if s.desktopDisplayUpdate != nil {
		return s.desktopDisplayUpdate(show)
	}
	return s.updateDesktopVisibility(show)
}

func (s *Server) changeBrowserVisibility(show bool) error {
	if s.macosGuest != nil || s.windowsGuest != nil {
		return nil
	}
	s.mu.Lock()
	ready, token := s.ready, s.credentials.Token
	s.mu.Unlock()
	if !ready {
		return nil
	}
	ctx, cancel := context.WithTimeout(s.lifetimeContext(), 5*time.Second)
	defer cancel()
	_, err := s.guestAPI(ctx, token, "POST", "/visibility", []byte(`{"visible":`+strconv.FormatBool(show)+`}`))
	return err
}
