package maclogin

import (
	"math"
	"strings"
	"testing"
	"time"
)

func maintenanceFixture() maintenanceWindow {
	return maintenanceWindow{Version: 1, Session: "607bceb6-c53f-4253-bad4-9c22b4ae7325", User: "trial", PID: 42, Window: 7,
		Bundle: "com.apple.systempreferences", Rect: [4]float64{300, 150, 1200, 900}, Display: [4]float64{0, 0, 1920, 1200}}
}

func TestMaintenanceWindowRequiresBoundIdentityAndVisibleRegion(t *testing.T) {
	valid := maintenanceFixture()
	if !valid.valid("trial") || valid.region() != [4]int{160, 80, 640, 480} {
		t.Fatal("verified settings window was rejected")
	}
	for _, change := range []func(*maintenanceWindow){
		func(w *maintenanceWindow) { w.User = "another-user" },
		func(w *maintenanceWindow) { w.Version = 0 },
		func(w *maintenanceWindow) { w.PID = 0 },
		func(w *maintenanceWindow) { w.Window = 0 },
		func(w *maintenanceWindow) { w.Bundle = "com.apple.finder" },
		func(w *maintenanceWindow) { w.Bundle = "com.example.authorizationhost" },
		func(w *maintenanceWindow) { w.Session = "" },
		func(w *maintenanceWindow) { w.Error = "requires_gui_login" },
		func(w *maintenanceWindow) { w.Error = "maintenance_window_obscured" },
		func(w *maintenanceWindow) { w.Rect[0] = -1 },
		func(w *maintenanceWindow) { w.Rect[2] = 80_000 },
		func(w *maintenanceWindow) { w.Rect[3] = math.NaN() },
		func(w *maintenanceWindow) { w.Display[2] = math.Inf(1) },
		func(w *maintenanceWindow) { w.Display[3] = 1080 },
	} {
		invalid := valid
		change(&invalid)
		if invalid.valid("trial") {
			t.Fatalf("accepted unverified maintenance target: %+v", invalid)
		}
	}
	if !valid.contains(160, 80) || !valid.contains(799, 559) || valid.contains(159, 80) || valid.contains(800, 559) || valid.contains(160, 560) {
		t.Fatal("maintenance input escaped the shown crop")
	}
}

func TestMaintenanceGeometryChangesWithWindowAndSession(t *testing.T) {
	first := maintenanceFixture()
	focused := first
	focused.PasswordFocused = true
	if focused.geometry() != first.geometry() {
		t.Fatal("a click that focuses a field invalidated ordinary typing")
	}
	for _, change := range []func(*maintenanceWindow){
		func(w *maintenanceWindow) { w.Window++ },
		func(w *maintenanceWindow) { w.PID++ },
		func(w *maintenanceWindow) { w.Rect[0]++ },
		func(w *maintenanceWindow) { w.Session = "1e128373-fcb9-42fd-88b2-2a6ca58cf3a1" },
	} {
		next := first
		change(&next)
		if next.geometry() == first.geometry() {
			t.Fatal("stale window input retained its authorization")
		}
	}
}

func TestMaintenancePasswordSelectionWorksOnceBeforeAccessibility(t *testing.T) {
	now := time.Now()
	window := maintenanceFixture()
	target := passwordTarget{maintenanceWindow: window, SelectionRequired: true}
	var selection passwordSelection
	if _, allowed := selection.consumeMaintenance(window, target, "trial", now); allowed {
		t.Fatal("maintenance password accepted without the user's selection")
	}
	selection.selectMaintenance(window, target, "trial", 200, 200, now)
	chosen, allowed := selection.consumeMaintenance(window, target, "trial", now.Add(time.Second))
	if !allowed || !window.permitsPassword(target, chosen, "trial", now.Add(2*time.Second)) {
		t.Fatal("AX-disabled maintenance could not use the selected Apple authorization panel")
	}
	if _, allowed := selection.consumeMaintenance(window, target, "trial", now.Add(2*time.Second)); allowed {
		t.Fatal("the same manual password selection was consumed twice")
	}
	if window.permitsPassword(target, chosen, "trial", now.Add(31*time.Second)) {
		t.Fatal("expired manual selection remained valid during password entry")
	}
}

func TestMaintenancePasswordRejectsChangedPanelAndSecureFocus(t *testing.T) {
	now := time.Now()
	window := maintenanceFixture()
	target := passwordTarget{maintenanceWindow: window, SelectionRequired: true}
	var selection passwordSelection
	selection.selectMaintenance(window, target, "trial", 200, 200, now)
	chosen, allowed := selection.consumeMaintenance(window, target, "trial", now)
	if !allowed {
		t.Fatal("could not establish the manual selection fixture")
	}
	for _, change := range []func(*passwordTarget){
		func(w *passwordTarget) { w.SelectionRequired = false },
		func(w *passwordTarget) { w.Window++ },
		func(w *passwordTarget) { w.PID++ },
		func(w *passwordTarget) { w.Rect[0]++ },
		func(w *passwordTarget) { w.Session = "1e128373-fcb9-42fd-88b2-2a6ca58cf3a1" },
		func(w *passwordTarget) { w.User = "another-user" },
		func(w *passwordTarget) { w.Bundle = "com.apple.finder" },
		func(w *passwordTarget) { w.Accessibility = true },
	} {
		changed := target
		change(&changed)
		if window.permitsPassword(changed, chosen, "trial", now.Add(time.Second)) {
			t.Fatal("a changed panel, session or known non-password focus accepted password input")
		}
	}
	ordinary := target
	ordinary.SelectionRequired = false
	selection.selectMaintenance(window, ordinary, "trial", 200, 200, now)
	if _, allowed := selection.consumeMaintenance(window, ordinary, "trial", now); allowed {
		t.Fatal("an ordinary System Settings window accepted manual password selection")
	}
	selection.selectMaintenance(window, target, "trial", 159, 80, now)
	if _, allowed := selection.consumeMaintenance(window, target, "trial", now); allowed {
		t.Fatal("a click outside the published window accepted password entry")
	}
	target.Accessibility, target.PasswordFocused = true, true
	if _, allowed := selection.consumeMaintenance(window, target, "trial", now); !allowed {
		t.Fatal("verified secure focus required an unnecessary manual selection")
	}
}

func TestMaintenanceProbeDistinguishesBlockedWindow(t *testing.T) {
	w := maintenanceWindow{Error: "maintenance_window_obscured", BlockingPID: 42, BlockingWindow: 7, BlockingBundle: "com.apple.notificationcenterui"}
	if message := w.probeMessage(); !strings.Contains(message, "通知中心 PID 42 窗口 7") {
		t.Fatal(message)
	}
	w.BlockingBundle = "unexpected\nwindow title"
	if strings.Contains(w.probeMessage(), "unexpected") {
		t.Fatal("invalid bundle identifier reported")
	}
	w.Error = "maintenance_window_unavailable"
	if strings.Contains(w.probeMessage(), "遮挡") {
		t.Fatal("a missing window was attributed to an obstruction")
	}
}
