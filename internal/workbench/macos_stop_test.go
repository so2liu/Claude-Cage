package workbench

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"al.essio.dev/pkg/shellescape"
	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/guestos"
	"github.com/sylvinus/agent-vm/internal/managedproxy"
	"golang.org/x/crypto/ssh"
)

func TestMacOSExplicitStopReleasesProxyAfterGuestStops(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("launchd proxy lifecycle")
	}
	for _, failGuest := range []bool{false, true} {
		t.Run(fmt.Sprintf("guest_failure_%t", failGuest), func(t *testing.T) {
			s := testServer(t)
			s.macosGuest = &guestos.MacOSProfile{User: "cage", Instance: "mac"}
			s.ready = true
			bin := t.TempDir()
			marker := filepath.Join(bin, "proxy-stopped")
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			launcher := "#!/bin/sh\nif [ \"$1\" = bootout ]; then printf stopped > " + shellescape.Quote(marker) + "; fi\nexit 0\n"
			if err := os.WriteFile(filepath.Join(bin, "launchctl"), []byte(launcher), 0700); err != nil {
				t.Fatal(err)
			}
			s.exe = filepath.Join(bin, "cage")
			code := 0
			if failGuest {
				code = 1
			}
			if err := os.WriteFile(s.exe, []byte(fmt.Sprintf("#!/bin/sh\nexit %d\n", code)), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(managedproxy.Dir(s.profile), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(managedproxy.Dir(s.profile), "managed-service.plist"), []byte("isolated test service"), 0600); err != nil {
				t.Fatal(err)
			}
			response := windowsBrowserRequestForTest(s, http.MethodPost, "/api/stop", []byte(`{}`))
			job := awaitJobIdle(t, &s.jobs)
			if response.Code != http.StatusOK || (job.Error != "") != failGuest {
				t.Fatalf("stop response %d, job %+v", response.Code, job)
			}
			_, err := os.Stat(marker)
			if failGuest && !os.IsNotExist(err) || !failGuest && err != nil {
				t.Fatalf("proxy cleanup must follow successful guest shutdown: %v", err)
			}
		})
	}
}

func TestMacOSStopStartPreservesUserNetworkIntent(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("native Mac control uses host OpenSSH")
	}
	for _, paused := range []bool{false, true} {
		t.Run(fmt.Sprintf("paused_%t", paused), func(t *testing.T) {
			guest := newMacWorkspaceGuest(t, func(_ ssh.Channel, command string) (uint32, bool) {
				return 0, strings.Contains(command, "uname -s") || strings.Contains(command, "/usr/sbin/systemsetup") || strings.Contains(command, "AppleLocale")
			})
			s := guest.s
			s.exe = filepath.Join(t.TempDir(), "cage")
			if err := os.WriteFile(s.exe, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := writePrivate(filepath.Join(s.profile, "network-intent.json"), networkIntent{Version: 1, Paused: paused, PauseVersion: 1}); err != nil {
				t.Fatal(err)
			}
			if err := s.restoreNetworkIntent(); err != nil {
				t.Fatal(err)
			}
			config, err := loadConfig(s.configPath)
			if err != nil {
				t.Fatal(err)
			}
			gate, err := cage.NewEgressGate(config)
			if err != nil {
				t.Fatal(err)
			}
			stop, err := cage.StartGateControl(s.configPath, s.instanceDir(), gate)
			if err != nil {
				t.Fatal(err)
			}
			defer stop()
			store, err := managedproxy.Load(s.profile)
			if err != nil {
				t.Fatal(err)
			}
			store.Selected = ""
			if err := store.Save(s.profile); err != nil {
				t.Fatal(err)
			}
			checkFailure := config.RequireManagedProxy(s.profile)
			if checkFailure == nil {
				t.Fatal("fixture did not require a fresh network check")
			}
			for _, route := range []string{"/api/stop", "/api/start"} {
				response := windowsBrowserRequestForTest(s, http.MethodPost, route, []byte(`{}`))
				job := awaitJobIdle(t, &s.jobs)
				wantCheck := route == "/api/start" && !paused
				if response.Code != http.StatusOK || wantCheck && !strings.Contains(job.Error, checkFailure.Error()) || !wantCheck && job.Error != "" {
					t.Fatalf("%s completed without the expected network decision: %d %+v", route, response.Code, job)
				}
				reloaded := reloadedIntentServer(t, s)
				if err := reloaded.restoreNetworkIntent(); err != nil || reloaded.networkPaused != paused {
					t.Fatalf("%s changed saved user pause: %t, %v", route, reloaded.networkPaused, err)
				}
			}
			if !s.ready || gate.Status().Mode != cage.GateClosed {
				t.Fatal("guest must be ready while fresh authorization remains absent")
			}
		})
	}
}
