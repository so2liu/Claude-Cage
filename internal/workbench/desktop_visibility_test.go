package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestBrowserVisibilityUsesGuestControllerAndIndependentDemand(t *testing.T) {
	s := testServer(t)
	s.ready = true
	changes := make(chan bool, 4)
	s.browserControl = browserClientForTest(t, s, browserTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost || r.URL.Path != "/visibility" {
			t.Errorf("unexpected browser visibility request: %s %s", r.Method, r.URL.Path)
		}
		var input struct {
			Visible bool `json:"visible"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		changes <- input.Visible
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"ok":true}`))}, nil
	}))
	desktop, err := s.desktopViews.acquire(func(bool) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer desktop()
	first, err := s.browserViews.acquire(s.changeBrowserVisibility)
	if err != nil {
		t.Fatal(err)
	}
	if !<-changes {
		t.Fatal("browser was not restored before viewing")
	}
	second, err := s.browserViews.acquire(s.changeBrowserVisibility)
	if err != nil {
		t.Fatal(err)
	}
	first()
	select {
	case <-changes:
		t.Fatal("one browser viewer hid another viewer's window")
	case <-time.After(600 * time.Millisecond):
	}
	second()
	select {
	case visible := <-changes:
		if visible {
			t.Fatal("browser stayed visible without its viewers")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Desktop viewing prevented the browser from hiding")
	}
}

func TestPhoneDesktopCloseReleasesOnlyPhoneDisplay(t *testing.T) {
	s := testServer(t)
	changes := make(chan bool, 4)
	update := func(show bool) error { changes <- show; return nil }
	computer, err := s.desktopViews.acquire(update)
	if err != nil {
		t.Fatal(err)
	}
	if !<-changes {
		t.Fatal("computer not shown")
	}
	phone, err := s.desktopViews.acquire(update)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	s.browserControl = browserClientForTest(t, s, browserTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/phone-desktop/close" {
			t.Fatalf("unexpected guest operation: %s", r.URL.Path)
		}
		closed = true
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	}))
	ctx, cancel := context.WithCancel(t.Context())
	b := &phoneSurfaceSession{id: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", ctx: ctx, cancel: cancel, stopEpoch: func() bool { return false },
		releaseDisplay: func() {
			if !closed {
				t.Error("display released before phone layout restored")
			}
			phone()
		}}
	s.phones.desktop.session = b
	s.phones.desktop.op.Lock()
	s.retirePhoneSurface(phoneDesktop, b)
	s.retirePhoneSurface(phoneDesktop, b)
	s.phones.desktop.op.Unlock()
	if !closed || b.releaseDisplay != nil || s.phones.desktop.session != nil {
		t.Fatal("phone display not retired")
	}
	select {
	case show := <-changes:
		t.Fatalf("phone close changed computer display to %t", show)
	case <-time.After(600 * time.Millisecond):
	}
	computer()
	select {
	case show := <-changes:
		if show {
			t.Fatal("last computer close did not hide")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("phone lease leaked")
	}
}

func TestDesktopViewsKeepAnotherViewerVisible(t *testing.T) {
	var demand displayViewDemand
	changes := make(chan bool, 4)
	update := func(show bool) error { changes <- show; return nil }
	first, err := demand.acquire(update)
	if err != nil {
		t.Fatal(err)
	}
	if !<-changes {
		t.Fatal("first viewer did not restore window")
	}
	second, err := demand.acquire(update)
	if err != nil {
		t.Fatal(err)
	}
	first()
	first()
	select {
	case show := <-changes:
		t.Fatalf("closing one viewer changed display to %t", show)
	case <-time.After(600 * time.Millisecond):
	}
	second()
	select {
	case show := <-changes:
		if show {
			t.Fatal("last viewer did not hide window")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("last viewer left display running")
	}
}

func TestDesktopNewViewerWaitsForPendingHide(t *testing.T) {
	var demand displayViewDemand
	hiding, finishHide := make(chan struct{}), make(chan struct{})
	cleanup := make(chan struct{})
	hides := 0
	update := func(show bool) error {
		if !show {
			hides++
			if hides == 1 {
				close(hiding)
				<-finishHide
			} else {
				close(cleanup)
			}
		}
		return nil
	}
	release, err := demand.acquire(update)
	if err != nil {
		t.Fatal(err)
	}
	release()
	<-hiding
	resumed := make(chan func(), 1)
	go func() { release, _ := demand.acquire(update); resumed <- release }()
	select {
	case <-resumed:
		close(finishHide)
		t.Fatal("new viewer resumed before old hide completed")
	case <-time.After(100 * time.Millisecond):
	}
	close(finishHide)
	release = <-resumed
	release()
	<-cleanup
}

func TestDesktopFailedRestoreReleasesItsViewer(t *testing.T) {
	var demand displayViewDemand
	if _, err := demand.acquire(func(bool) error { return errors.New("guest unavailable") }); err == nil {
		t.Fatal("failed restore accepted")
	}
	changes := make(chan bool, 4)
	release, err := demand.acquire(func(show bool) error { changes <- show; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !<-changes {
		t.Fatal("restore was not retried")
	}
	release()
	select {
	case show := <-changes:
		if show {
			t.Fatal("failed restore retained a display lease")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("viewer leaked after failure")
	}
}
