package workbench

import (
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestMacOSSettingsApplyAndRejectFailures(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "applied"
		if fail {
			name = "timezone_failure"
		}
		t.Run(name, func(t *testing.T) {
			var mu sync.Mutex
			var calls []string
			g := newMacWorkspaceGuest(t, func(_ ssh.Channel, command string) (uint32, bool) {
				if !strings.Contains(command, "/usr/sbin/systemsetup") && !strings.Contains(command, "AppleLocale") {
					return 0, false
				}
				mu.Lock()
				calls = append(calls, command)
				mu.Unlock()
				if fail && strings.Contains(command, "/usr/sbin/systemsetup") {
					return 1, true
				}
				return 0, true
			})
			cfg, err := loadConfig(g.s.configPath)
			if err != nil {
				t.Fatal(err)
			}
			cfg.Timezone = "Etc/UTC"
			cfg.Locale = "en_US.UTF-8"
			err = g.s.applyMacOSGuestSettings(t.Context(), cfg)
			if (err != nil) != fail {
				t.Fatalf("failure propagation: %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			want := 2
			if fail {
				want = 1
			}
			if len(calls) != want {
				t.Fatalf("got %d operations, want %d", len(calls), want)
			}
			if !strings.Contains(calls[0], "-settimezone GMT") {
				t.Fatalf("UTC alias was not accepted by systemsetup: %s", calls[0])
			}
			before := len(calls)
			cfg.Timezone = "Etc/UTC; touch /tmp/injected"
			if g.s.applyMacOSGuestSettings(t.Context(), cfg) == nil {
				t.Fatal("invalid system setting accepted")
			}
			if len(calls) != before {
				t.Fatal("invalid input reached guest")
			}
		})
	}
}

func TestMacOSDNSResetReportsGuestFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "success"
		if fail {
			name = "guest_failure"
		}
		t.Run(name, func(t *testing.T) {
			g := newMacWorkspaceGuest(t, func(_ ssh.Channel, command string) (uint32, bool) {
				if !strings.Contains(command, "/usr/bin/dscacheutil -flushcache") || !strings.Contains(command, "/usr/bin/killall -HUP mDNSResponder") {
					return 0, false
				}
				if fail {
					return 1, true
				}
				return 0, true
			})
			if err := g.s.resetMacOSDNSCache(t.Context()); (err != nil) != fail {
				t.Fatalf("DNS reset error=%v, failure=%t", err, fail)
			}
		})
	}
}
