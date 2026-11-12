package workbench

import (
	"context"
	"encoding/json"
	"github.com/sylvinus/agent-vm/internal/host"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/managedproxy"
)

func testServer(t *testing.T) *Server {
	t.Helper()
	profile := t.TempDir()
	path := filepath.Join(profile, "config.json")
	c := cage.DefaultConfig()
	p, _ := managedproxy.Load(profile)
	p.Auth = strings.Repeat("secret", 8)
	if err := p.Import("test", "", []byte(`proxies: [{name: qa, type: socks5, server: 127.0.0.1, port: 1}]`), "source", 0, ""); err != nil {
		t.Fatal(err)
	}
	p.Selected = p.Sources[0].Nodes[0].ID
	p.Revision = "test-revision"
	if err := p.Save(profile); err != nil {
		t.Fatal(err)
	}
	c.ProxyURL = p.Endpoint()
	c.ProxyRevision = p.Revision
	if e := writePrivate(path, c); e != nil {
		t.Fatal(e)
	}
	// Non-account tests exercise an already unlocked workbench. Account tests
	// replace this in-memory fixture with the real persisted account store.
	account := &localAccount{path: filepath.Join(profile, "account.json"), record: &accountRecord{Version: 1, Username: "Test"}}
	account.setKey(make([]byte, 32))
	t.Cleanup(account.lock)
	return &Server{account: account, profile: profile, configPath: path, origin: "http://localhost:17777", token: strings.Repeat("a", 64), session: strings.Repeat("b", 64), preferences: Preferences{Keyboard: "mac"}}
}

// Fixtures for network policy tests need a published state without executing
// a real environment operation. Production code submits through submitJob.
func (s *Server) setJobForTest(job Job) {
	s.jobs.mu.Lock()
	defer s.jobs.mu.Unlock()
	s.jobs.state = job
}

func request(s *Server, method, path, origin, body string, auth bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, s.origin+path, strings.NewReader(body))
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	r.Header.Set("Content-Type", "application/json")
	if auth {
		r.AddCookie(&http.Cookie{Name: s.cookieName(), Value: s.session})
	}
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, r)
	return w
}
func TestHostManagementBoundary(t *testing.T) {
	s := testServer(t)
	if w := request(s, "GET", "/api/settings", "", "", false); w.Code != 401 {
		t.Fatalf("unauthenticated settings: %d", w.Code)
	}
	for _, origin := range []string{"", "http://127.0.0.1:18080", "https://attacker.example"} {
		if w := request(s, "POST", "/api/start", origin, "{}", true); w.Code != 403 {
			t.Fatalf("cross-origin %q: %d", origin, w.Code)
		}
	}
	r := httptest.NewRequest("GET", "http://rebind.example:17777/api/status", nil)
	r.AddCookie(&http.Cookie{Name: s.cookieName(), Value: s.session})
	w := httptest.NewRecorder()
	s.handler().ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("DNS rebinding Host accepted: %d", w.Code)
	}
	w = request(s, "GET", "/api/settings", "", "", true)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), strings.Repeat("secret", 8)) || strings.Contains(w.Body.String(), "proxy_url") {
		t.Fatal("password returned to UI")
	}
	if w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("CORS unexpectedly enabled")
	}
	if !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-src 'none'") {
		t.Fatal("guest frames allowed")
	}
}
func TestBootstrapRequiresSecretAndIssuesHostOnlyCookie(t *testing.T) {
	s := testServer(t)
	if w := request(s, "POST", "/api/session", s.origin, `{"token":"wrong"}`, false); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := request(s, "POST", "/api/session", s.origin, `{"token":"`+s.token+`"}{}`, false); w.Code != 400 {
		t.Fatal("extra JSON accepted")
	}
	ticket := s.issueTicket()
	w := request(s, "POST", "/api/session", s.origin, `{"token":"`+ticket+`"}`, false)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	if replay := request(s, "POST", "/api/session", s.origin, `{"token":"`+ticket+`"}`, false); replay.Code != 401 {
		t.Fatal("bootstrap ticket may be replayed")
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatal("missing cookie")
	}
	c := cookies[0]
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Domain != "" {
		t.Fatalf("unsafe cookie attributes: %+v", c)
	}
}
func TestSettingsPreserveSecretsAndRejectHostSharing(t *testing.T) {
	s := testServer(t)
	old, e := cage.Load(s.configPath)
	if e != nil {
		t.Fatal(e)
	}
	input := settingsView(old, s.preferences)
	input.Keyboard = "native"
	next, _, e := proposedSettings(input, old, s.profile)
	if e != nil {
		t.Fatal(e)
	}
	if next.ProxyURL != old.ProxyURL {
		t.Fatal("blank password erased existing authentication")
	}
	input.Workspace = s.profile
	if _, _, e = proposedSettings(input, old, s.profile); e == nil {
		t.Fatal("profile may be shared")
	}
	input.Workspace = filepath.Dir(s.profile)
	if _, _, e = proposedSettings(input, old, s.profile); e == nil {
		t.Fatal("profile ancestor may be shared")
	}
	workspace := t.TempDir()
	link := filepath.Join(workspace, "private-link")
	if e = os.Symlink(s.profile, link); e != nil {
		t.Fatal(e)
	}
	input.Workspace = link
	if _, _, e = proposedSettings(input, old, s.profile); e == nil {
		t.Fatal("symlink bypassed share guard")
	}
}
func TestInvalidSettingsDoNotReplaceCurrentProfile(t *testing.T) {
	s := testServer(t)
	before, _ := os.ReadFile(s.configPath)
	old, _ := cage.Load(s.configPath)
	input := settingsView(old, s.preferences)
	input.Timezone = "Invented/Invalid"
	body, _ := json.Marshal(input)
	w := request(s, "POST", "/api/settings", s.origin, string(body), true)
	if w.Code != 400 {
		t.Fatal(w.Code)
	}
	after, _ := os.ReadFile(s.configPath)
	if string(before) != string(after) {
		t.Fatal("invalid save changed profile")
	}
}
func TestKeyboardPreferenceDoesNotRestartOrChangeNetwork(t *testing.T) {
	s := testServer(t)
	old, _ := cage.Load(s.configPath)
	s.ready = true
	p := s.preferences
	p.Keyboard = "native"
	if e := s.apply(savedProfile{old, p}, true); e != nil {
		t.Fatal(e)
	}
	after, _ := cage.Load(s.configPath)
	if after != old || !s.ready {
		t.Fatal("keyboard-only save changed runtime")
	}
	b, _ := os.ReadFile(filepath.Join(s.profile, "workbench.json"))
	var saved Preferences
	_ = json.Unmarshal(b, &saved)
	if saved.Keyboard != "native" {
		t.Fatal("preference not persisted")
	}
	if host.CheckPrivateFile(s.configPath) != nil {
		t.Fatal("profile permissions")
	}
}

func TestDisplayPreferenceAppliesAtNextLaunch(t *testing.T) {
	s := testServer(t)
	cfg, err := cage.Load(s.configPath)
	if err != nil {
		t.Fatal(err)
	}
	s.ready = true
	s.credentials.DisplayScale = 2
	p := s.preferences
	p.BalancedDisplay = true
	if err := s.apply(savedProfile{cfg, p}, true); err != nil {
		t.Fatal(err)
	}
	// Reconnecting before the next launch must still size the active 2x display.
	w := request(s, "GET", "/api/vnc-credentials", "", "", true)
	var active struct {
		Scale   float64 `json:"display_scale"`
		Dynamic bool    `json:"dynamic_scale"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &active) != nil || active.Scale != 2 || active.Dynamic {
		t.Fatal("reconnect lost the running display density")
	}
	// A guest with a settings manager lets each viewer choose its density.
	s.credentials.DynamicScale = true
	w = request(s, "GET", "/api/vnc-credentials", "", "", true)
	if json.Unmarshal(w.Body.Bytes(), &active) != nil || !active.Dynamic {
		t.Fatal("viewer was not told that the display follows its screen")
	}
	// A client that omits the new preference must preserve the user's choice.
	settings := request(s, "GET", "/api/settings", "", "", true)
	var payload map[string]any
	if err = json.Unmarshal(settings.Body.Bytes(), &payload); err != nil || payload["balanced_display"] != true {
		t.Fatal("saved display preference unavailable")
	}
	delete(payload, "balanced_display")
	body, _ := json.Marshal(payload)
	r := httptest.NewRequest("POST", "/api/settings", strings.NewReader(string(body)))
	r.Header.Set("Content-Type", "application/json")
	next, ok := s.proposed(httptest.NewRecorder(), r)
	if !ok || !next.Preferences.BalancedDisplay || next.Config != cfg {
		t.Fatal("legacy settings save changed the display preference or network configuration")
	}
}

func TestLocationProbeHasNoDirectFallback(t *testing.T) {
	c := cage.DefaultConfig()
	c.ProxyURL = "socks5://127.0.0.1:1"
	if _, e := probeLocation(context.Background(), c); e == nil {
		t.Fatal("location probe bypassed unavailable proxy")
	}
}
func TestLocationLiveIntegration(t *testing.T) {
	path := os.Getenv("CAGE_TEST_CONFIG")
	if path == "" {
		t.Skip("explicit proxy profile required")
	}
	c, e := cage.Load(path)
	if e != nil {
		t.Fatal(e)
	}
	loc, e := probeLocation(context.Background(), c)
	if e != nil {
		t.Fatal(e)
	}
	if loc.IP == "" || loc.Timezone == "" {
		t.Fatal("missing location")
	}
	t.Log("Proxy geolocation returned a valid IP and IANA timezone; sensitive values omitted")
}

func TestRestoreCannotReplaceManagedEndpoint(t *testing.T) {
	s := testServer(t)
	s.ready = true
	old, _ := cage.Load(s.configPath)
	bad := old
	bad.ProxyURL = "socks5://127.0.0.1:1"
	if e := s.apply(savedProfile{bad, s.preferences}, true); e != nil {
		t.Fatal(e)
	}
	after, _ := loadConfig(s.configPath)
	if after != old || !s.ready {
		t.Fatal("restore changed the managed endpoint or interrupted the environment")
	}
}

func TestSSHIsolationOverridesLocalClientSettings(t *testing.T) {
	if _, err := os.Stat(sshExecutable()); err != nil {
		t.Skip("OpenSSH is required")
	}
	s := testServer(t)
	s.preferences.Workspace = t.TempDir()
	dir := filepath.Join(s.profile, "state", "lima", cage.InstanceName(s.profile, s.environmentDir()))
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	config := "Host *\n  BatchMode no\n  ForwardAgent yes\n  ForwardX11 yes\n  PermitLocalCommand yes\n  EscapeChar ~\n  PasswordAuthentication yes\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh.config"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CAGE_TEST_HOST_SECRET", "must-not-cross-the-boundary")
	cmd := s.sshCommand(context.Background(), "-G", "lima-test")
	if cmd.Env == nil {
		t.Fatal("SSH inherits the host environment")
	}
	for _, v := range cmd.Env {
		if strings.HasPrefix(v, "CAGE_TEST_HOST_SECRET=") {
			t.Fatal("host secret forwarded")
		}
	}
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	effective := strings.ToLower(string(out))
	for _, expected := range []string{"batchmode yes", "forwardagent no", "forwardx11 no", "permitlocalcommand no", "escapechar none", "passwordauthentication no"} {
		if !strings.Contains(effective, expected) {
			t.Errorf("SSH effective configuration lacks %q", expected)
		}
	}
}
func TestImplicitHostTimezonesAreRejected(t *testing.T) {
	for _, zone := range []string{"", "Local"} {
		c := cage.DefaultConfig()
		c.Timezone = zone
		if c.Validate() == nil {
			t.Errorf("implicit timezone %q accepted", zone)
		}
	}
}
