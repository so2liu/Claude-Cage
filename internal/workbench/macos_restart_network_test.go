package workbench

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/managedproxy"
	"golang.org/x/crypto/ssh"
)

func TestMacOSStartAndControllerReconnectRequireFreshNetworkCheck(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("native Mac startup uses the host shell and OpenSSH")
	}
	for _, tc := range []struct {
		name                 string
		start, paused, setup bool
	}{
		{"cold start", true, false, false},
		{"cold start retains pause", true, true, false},
		{"first setup starts offline", true, false, true},
		{"controller adopts running guest", false, false, false},
		{"controller adopts paused guest", false, true, false},
		{"controller adopts unfinished setup", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newMacWorkspaceGuest(t, func(_ ssh.Channel, command string) (uint32, bool) {
				return 0, strings.Contains(command, "uname -s") || strings.Contains(command, "/usr/sbin/systemsetup") || strings.Contains(command, "AppleLocale")
			})
			s := g.s
			s.ready = false
			if tc.setup {
				s.macosGuest.SetupID = "awaiting-first-setup"
			}
			if err := writePrivate(filepath.Join(s.profile, "network-intent.json"), networkIntent{Version: 1, Paused: tc.paused, PauseVersion: 7}); err != nil {
				t.Fatal(err)
			}
			if err := s.restoreNetworkIntent(); err != nil {
				t.Fatal(err)
			}
			cfg, err := loadConfig(s.configPath)
			if err != nil {
				t.Fatal(err)
			}
			gate, err := cage.NewEgressGate(cfg)
			if err != nil {
				t.Fatal(err)
			}
			stop, err := cage.StartGateControl(s.configPath, s.instanceDir(), gate)
			if err != nil {
				t.Fatal(err)
			}
			defer stop()
			if err = gate.BeginCheck(); err != nil {
				t.Fatal(err)
			}
			grantTestGate(t, gate)
			// Preserve the same configuration fingerprint and old report, but
			// reject the new check before any proxy process or external traffic.
			store, err := managedproxy.Load(s.profile)
			if err != nil {
				t.Fatal(err)
			}
			store.Selected = ""
			if err = store.Save(s.profile); err != nil {
				t.Fatal(err)
			}
			checkFailure := cfg.RequireManagedProxy(s.profile)
			if checkFailure == nil {
				t.Fatal("fixture still has a selected exit")
			}
			wantCheck := !tc.paused && !tc.setup
			if tc.start {
				marker := filepath.Join(s.instanceDir(), "cage-running.json")
				if err := os.Remove(marker); err != nil {
					t.Fatal(err)
				}
				s.exe = filepath.Join(t.TempDir(), "cage")
				script := fmt.Sprintf("#!/bin/sh\n[ \"$*\" = 'guest-os macos start' ] || exit 1\nprintf '{\"pid\":%d}' > '%s'\n", os.Getpid(), marker)
				if err := os.WriteFile(s.exe, []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
				s.setJobForTest(Job{ID: "starting", Kind: "start", Running: true})
				err := s.startMacOSGuest()
				if wantCheck && (err == nil || err.Error() != checkFailure.Error()) || !wantCheck && err != nil {
					t.Fatalf("start result = %v, want fresh check = %t", err, wantCheck)
				}
			} else {
				s.op.Lock()
				err := s.attachMacOSGuest()
				job := s.jobs.snapshot()
				s.op.Unlock()
				defer s.jobs.close()
				if err != nil {
					t.Fatal(err)
				}
				if (job.Running && job.Kind == "network-recovery") != wantCheck {
					t.Fatalf("controller reconnect job = %+v, want fresh check = %t", job, wantCheck)
				}
				if wantCheck {
					if result := awaitJobIdle(t, &s.jobs); result.Error != checkFailure.Error() {
						t.Fatalf("reconnect completed without the expected proxy check: %+v", result)
					}
				}
			}
			if state := gate.Status(); !s.ready || state.Mode != cage.GateClosed || !time.Now().After(state.Until) || s.networkPaused != tc.paused || s.safety.pauseVersion != 7 {
				t.Fatalf("ready=%t mode=%s paused=%t generation=%d", s.ready, state.Mode, s.networkPaused, s.safety.pauseVersion)
			}
		})
	}
}
