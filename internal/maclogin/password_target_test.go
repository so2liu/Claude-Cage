package maclogin

import (
	"testing"
	"time"
)

func TestFirstAuthorizationUsesFreshSelectedSystemPanelBeforeAccessibility(t *testing.T) {
	now := time.Now()
	target := passwordTarget{maintenanceWindow: maintenanceFixture(), SelectionRequired: true}
	selected := passwordSelection{Geometry: target.geometry(), X: 200, Y: 200, Clicked: now}
	if !selected.permits(target, "trial", now.Add(time.Second)) {
		t.Fatal("first authorization required the permission it is preparing")
	}
	for _, test := range []struct {
		name   string
		change func(*passwordTarget, *passwordSelection)
	}{
		{"no selection", func(_ *passwordTarget, s *passwordSelection) { *s = passwordSelection{} }},
		{"expired selection", func(_ *passwordTarget, s *passwordSelection) { s.Clicked = now.Add(-31 * time.Second) }},
		{"future selection", func(_ *passwordTarget, s *passwordSelection) { s.Clicked = now.Add(time.Second) }},
		{"outside panel", func(_ *passwordTarget, s *passwordSelection) { s.X = 159 }},
		{"replaced window", func(w *passwordTarget, _ *passwordSelection) { w.Window++ }},
		{"moved window", func(w *passwordTarget, _ *passwordSelection) { w.Rect[0]++ }},
		{"another process", func(w *passwordTarget, _ *passwordSelection) { w.PID++ }},
		{"another GUI session", func(w *passwordTarget, _ *passwordSelection) { w.Session = "1e128373-fcb9-42fd-88b2-2a6ca58cf3a1" }},
		{"another app", func(w *passwordTarget, _ *passwordSelection) { w.Bundle = "com.apple.finder" }},
		{"ordinary settings window", func(w *passwordTarget, _ *passwordSelection) { w.SelectionRequired = false }},
		{"known non-password focus", func(w *passwordTarget, _ *passwordSelection) { w.Accessibility = true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			current, selection := target, selected
			test.change(&current, &selection)
			if selection.permits(current, "trial", now) {
				t.Fatal("unverified password target accepted")
			}
		})
	}
}

func TestAuthorizedAccessibilityRequiresSecureFocusForBothAppleHosts(t *testing.T) {
	for _, bundle := range []string{"com.apple.systempreferences", "com.apple.authorizationhost", "com.apple.SecurityAgent"} {
		target := passwordTarget{maintenanceWindow: maintenanceFixture(), Accessibility: true}
		target.Bundle, target.PasswordFocused = bundle, true
		if !(passwordSelection{}).permits(target, "trial", time.Now()) {
			t.Fatalf("verified secure field rejected in %s", bundle)
		}
		target.PasswordFocused = false
		if (passwordSelection{}).permits(target, "trial", time.Now()) {
			t.Fatal("non-password field accepted")
		}
	}
}
