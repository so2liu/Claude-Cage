package workbench

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sylvinus/agent-vm/internal/cage"
)

func TestBrowserExitRetryUsesFreshSampleAndKeepsFailureEvidence(t *testing.T) {
	for _, condition := range []string{"recovers", "unavailable", "settings mismatch", "invalid address", "canceled", "fault drill"} {
		t.Run(condition, func(t *testing.T) {
			s := testServer(t)
			cfg := cage.DefaultConfig()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != "/audit" {
					t.Errorf("unexpected browser request: %s %s", r.Method, r.URL.Path)
				}
				count := requests.Add(1)
				v := browserObservation{Timezone: cfg.Timezone, ExpectedTimezone: cfg.Timezone, Language: cfg.BrowserLanguage, Platform: "Linux x86_64", Network: networkObservation{Status: 503}}
				switch condition {
				case "recovers":
					if count == 2 {
						v.Network = networkObservation{Status: 200, IP: "8.8.8.8"}
					}
				case "settings mismatch":
					v.Language = "wrong-language"
				case "invalid address":
					v.Network = networkObservation{Status: 200}
				case "canceled":
					cancel()
				}
				_ = json.NewEncoder(w).Encode(v)
			}))
			defer server.Close()
			s.browserControl = newBrowserClient(t.Context())
			t.Cleanup(s.browserControl.close)
			s.browserControl.http.Transport.(*http.Transport).DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(server.URL, "http://"))
			}
			var v browserObservation
			var err error
			if condition == "fault drill" {
				v, err = s.browserObservation(ctx, cfg, true)
			} else {
				v, err = s.browserExitObservation(ctx, cfg)
			}
			wantRequests := int32(1)
			if condition == "recovers" || condition == "unavailable" {
				wantRequests = 2
			}
			if requests.Load() != wantRequests {
				t.Fatalf("requests=%d want=%d", requests.Load(), wantRequests)
			}
			if condition == "canceled" {
				if ctx.Err() == nil || err == nil {
					t.Fatal("canceled check continued", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if condition == "recovers" {
				if v.Network.Status != 200 || v.Network.IP != "8.8.8.8" {
					t.Fatal("retry did not replace the failed sample", v)
				}
			} else if condition == "unavailable" {
				good := networkObservation{Status: 200, IP: "8.8.8.8"}
				message := incompleteExitSamples(good.IP, nil, good, nil, v.Network, nil)
				if !strings.Contains(message, "浏览器：检测通道返回 HTTP 503") {
					t.Fatal("retry concealed the failure", message)
				}
			}
		})
	}
}

func TestStartJobDistinguishesNetworkFailureFromBootFailure(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{networkCheckError{errors.New("联网检查未通过：浏览器 HTTP 503，外网保持关闭")}, "环境已启动，联网检查未通过"},
		{errors.New("虚拟机未能启动"), "启动环境失败"},
		{networkCheckError{context.Canceled}, "已取消"},
	} {
		var manager jobManager
		var operation sync.Mutex
		if _, err := manager.submit(&operation, "start", 0, nil, nil, func() error { return tc.err }); err != nil {
			t.Fatal(err)
		}
		job := awaitJobIdle(t, &manager)
		if job.Message != tc.want || (tc.want != "已取消" && job.Error == "") {
			t.Fatalf("wrong startup result: %+v", job)
		}
	}
}

func TestSettingsJobRetainsSaveWhenNetworkCheckFails(t *testing.T) {
	s := testServer(t)
	saved := savedProfile{Config: cage.DefaultConfig(), Preferences: s.preferences}
	saved.Config.ExitPolicy = cage.ExitPolicy{Mode: "dynamic", Scope: "country", CountryCode: "US"}
	_, err := s.submitJob("settings", func() error {
		if err := s.apply(saved, false); err != nil {
			return err
		}
		return networkCheckError{errors.New("检测通道 HTTP 503")}
	})
	if err != nil {
		t.Fatal(err)
	}
	job := awaitJobIdle(t, &s.jobs)
	cfg, err := cage.Load(s.configPath)
	if err != nil || cfg.ExitPolicy != saved.Config.ExitPolicy || !job.SettingsSaved || job.Message != "设置已保存，环境已启动，联网检查未通过" || !strings.Contains(job.Error, "503") {
		t.Fatalf("persisted settings and later network error: cfg=%v job=%+v err=%v", cfg.ExitPolicy, job, err)
	}
}
