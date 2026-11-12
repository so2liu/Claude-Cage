package workbench

import (
	"bufio"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/kballard/go-shellquote"
	"github.com/pkg/sftp"
	"github.com/sylvinus/agent-vm/internal/guestos"
	"golang.org/x/crypto/ssh"
)

// This guest substitute uses the real bound OpenSSH transport and the confined
// host SFTP server. FSKit approval and native application execution are simulated.
type macWorkspaceGuest struct {
	s               *Server
	hostPath        string
	guestPath       string
	approved        atomic.Bool
	resourceTicks   atomic.Uint32
	resourceFailure atomic.Bool
	mu              sync.Mutex
	files           *sftp.Client
	command         func(ssh.Channel, string) (uint32, bool)
}

func newMacWorkspaceGuest(t *testing.T, commands ...func(ssh.Channel, string) (uint32, bool)) *macWorkspaceGuest {
	t.Helper()
	if _, err := os.Stat(sshExecutable()); err != nil {
		t.Skip("OpenSSH is unavailable")
	}
	s := testServer(t)
	s.macosGuest = &guestos.MacOSProfile{Instance: "mac", User: "cage"}
	hostPath, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = writePrivate(filepath.Join(s.profile, "environment.json"), map[string]string{"directory": hostPath}); err != nil {
		t.Fatal(err)
	}
	s.preferences.Workspace, s.ready = hostPath, true
	guest := &macWorkspaceGuest{s: s, hostPath: hostPath, guestPath: macOSProjectPath("cage", hostPath)}
	if len(commands) > 0 {
		guest.command = commands[0]
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{NoClientAuth: true}
	config.AddHostKey(signer)
	if err = os.MkdirAll(s.instanceDir(), 0700); err != nil {
		t.Fatal(err)
	}
	identity, err := ssh.MarshalPrivateKey(key, "workspace-fixture")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join(s.profile, "state", "lima", "_config"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(s.profile, "state", "lima", "_config", "user"), pem.EncodeToMemory(identity), 0600); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"state/lima/mac/ssh.config": fmt.Sprintf("Port %d\nUser cage\n", listener.Addr().(*net.TCPAddr).Port),
		"macos-known-hosts":         "cage-macos " + string(ssh.MarshalAuthorizedKey(signer.PublicKey())),
	} {
		if err = os.WriteFile(filepath.Join(s.profile, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err = writePrivate(filepath.Join(s.instanceDir(), "cage-running.json"), map[string]int{"pid": os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	var active sync.WaitGroup
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			active.Add(1)
			go func() {
				defer active.Done()
				server, channels, requests, err := ssh.NewServerConn(conn, config)
				if err != nil {
					conn.Close()
					return
				}
				defer server.Close()
				go ssh.DiscardRequests(requests)
				for incoming := range channels {
					channel, requests, err := incoming.Accept()
					if err != nil {
						return
					}
					for request := range requests {
						if request.Type != "exec" {
							request.Reply(true, nil)
							continue
						}
						var command struct{ Command string }
						if ssh.Unmarshal(request.Payload, &command) != nil {
							request.Reply(false, nil)
							break
						}
						request.Reply(true, nil)
						go ssh.DiscardRequests(requests)
						status := guest.execute(channel, command.Command)
						channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
						break
					}
					channel.Close()
				}
			}()
		}
	}()
	t.Cleanup(func() {
		s.beginClose()
		s.terminals.close()
		s.closeWindowsBrowser()
		s.closeProjectShares()
		listener.Close()
		<-acceptDone
		active.Wait()
	})
	return guest
}

func (g *macWorkspaceGuest) execute(channel ssh.Channel, command string) uint32 {
	if g.command != nil {
		if status, handled := g.command(channel, command); handled {
			return status
		}
	}
	args, err := shellquote.Split(command)
	if err != nil {
		return 1
	}
	if strings.HasPrefix(command, "exec /bin/bash --noprofile --norc -c ") {
		command = args[len(args)-1]
		args, err = shellquote.Split(command)
		if err != nil {
			return 1
		}
	}
	switch {
	case strings.HasPrefix(command, "exec /usr/local/libexec/cage-cli/bin/python3 -I -c "):
		if g.resourceFailure.Load() {
			fmt.Fprintln(channel, `{}`)
			return 0
		}
		ticks := g.resourceTicks.Add(10)
		json.NewEncoder(channel).Encode(macOSResourceSample{Version: 1, Total: 4 << 30, Available: 2 << 30,
			Free: 1 << 30, Inactive: 1 << 30, CPUs: 2, Boot: 10, CPUTicks: [4]uint32{ticks, 10, 80 + ticks, 0}})
		return 0
	case command == "/usr/bin/open -a /Applications/CageProjectAccess.app":
		return 0
	case strings.HasPrefix(command, "set -e;"):
		return 0
	case strings.HasPrefix(command, "exec /usr/local/libexec/cage-sshfs "):
		files, err := sftp.NewClientPipe(channel, channel)
		if err != nil {
			return 1
		}
		g.mu.Lock()
		g.files = files
		g.mu.Unlock()
		_ = files.Wait()
		return 0
	case len(args) == 5 && args[0] == macProjectAgent && args[1] == "probe":
		if !g.approved.Load() {
			return 75
		}
		g.mu.Lock()
		files := g.files
		g.mu.Unlock()
		if files == nil || args[2] != g.guestPath {
			return 1
		}
		file, err := files.Open(filepath.Join(g.hostPath, args[3]))
		if err != nil {
			return 1
		}
		body, err := io.ReadAll(file)
		file.Close()
		if err != nil || string(body) != args[4] {
			return 1
		}
		fmt.Fprint(channel, "OK")
		return 0
	case strings.HasPrefix(command, "/sbin/mount |"):
		fmt.Fprintf(channel, "macfuse://3c7e13b5-dc9f-4d09-b7c3-707987db4e34 on %s (fskit, nodev, nosuid, mounted by cage)\n", g.guestPath)
		return 0
	case strings.HasPrefix(command, "if test "):
		return 0
	case strings.HasPrefix(command, "exec /usr/local/libexec/cage-macos-terminal "):
		if len(args) != 3 || args[2] != g.guestPath || !g.approved.Load() {
			return 1
		}
		fmt.Fprintln(channel, "shared-workspace-ready")
		var input strings.Builder
		buffer := make([]byte, 1024)
		for {
			n, err := channel.Read(buffer)
			if err != nil {
				return 0
			}
			input.Write(buffer[:n])
			if strings.Contains(input.String(), "fixture-boundary:guest") {
				g.mu.Lock()
				files := g.files
				g.mu.Unlock()
				file, err := files.OpenFile(filepath.Join(g.hostPath, ".cage-share-probe-boundary"), os.O_WRONLY|os.O_TRUNC)
				if err != nil {
					return 1
				}
				_, err = file.Write([]byte("fixture-boundary:guest"))
				file.Close()
				if err != nil {
					return 1
				}
				return 0
			}
		}
	case strings.Contains(command, "macos-native-browser.mjs"):
		scanner := bufio.NewScanner(channel)
		for scanner.Scan() {
			var request struct {
				Path  string
				Input map[string]any
			}
			if json.Unmarshal(scanner.Bytes(), &request) != nil {
				return 1
			}
			json.NewEncoder(channel).Encode(map[string]any{"ok": true, "workspace": request.Input["workspace"]})
		}
		return 0
	default:
		fmt.Fprintln(channel.Stderr(), "unexpected fixture command")
		return 1
	}
}

func TestMacOSProjectOpenWithRequiresSSHFSCapability(t *testing.T) {
	for _, capability := range []struct {
		name   string
		output string
		want   bool
	}{
		{name: "legacy helper"},
		{name: "supported helper", output: "\nCAGE_SSHFS_OPENWITH\n", want: true},
		{name: "unrelated diagnostic", output: "unknown CAGE_SSHFS_OPENWITH option\n"},
	} {
		t.Run(capability.name, func(t *testing.T) {
			mounts := make(chan string, 1)
			guest := newMacWorkspaceGuest(t, func(channel ssh.Channel, command string) (uint32, bool) {
				if strings.Contains(command, "CAGE_SSHFS_OPENWITH") {
					fmt.Fprint(channel, capability.output)
					return 0, true
				}
				if strings.HasPrefix(command, "exec /usr/local/libexec/cage-sshfs ") {
					mounts <- command
				}
				return 0, false
			})
			guest.approved.Store(true)
			if err := guest.s.ensureMacOSProjectShare(guest.hostPath); err != nil {
				t.Fatal(err)
			}
			command := <-mounts
			args, err := shellquote.Split(command)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, arg := range args {
				if strings.HasPrefix(arg, "cage_openwith=") {
					found = true
					if arg != "cage_openwith=/Users/cage/Library/Caches/dev.cage.GuestClaudeWindow/openwith.data" {
						t.Fatalf("unexpected guest metadata path %q", arg)
					}
				}
			}
			if found != capability.want {
				t.Fatalf("OpenWith option present = %v, want %v", found, capability.want)
			}
		})
	}
}

func TestMacOSDefaultWorkspaceUsesApprovedSharedProject(t *testing.T) {
	guest := newMacWorkspaceGuest(t)
	s := guest.s
	marker := filepath.Join(guest.hostPath, "keep.txt")
	if err := os.WriteFile(marker, []byte("original-host-data"), 0600); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"workspace": guest.hostPath, "activate": true})
	response := windowsBrowserRequestForTest(s, http.MethodPost, "/api/projects/open", body)
	var pending struct{ Workspace, Approval string }
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &pending) != nil || pending.Approval == "" || pending.Workspace != guest.hostPath {
		t.Fatalf("default workspace bypassed project approval: %d %s", response.Code, response.Body.String())
	}
	if s.projectShares[guest.hostPath].detach != nil {
		t.Fatal("unapproved share was accepted")
	}
	guest.approved.Store(true)
	response = windowsBrowserRequestForTest(s, http.MethodPost, "/api/projects/open", body)
	if response.Code != 200 || strings.Contains(response.Body.String(), "approval") || s.projectShares[guest.hostPath].detach == nil {
		t.Fatalf("approved default workspace failed: %d %s", response.Code, response.Body.String())
	}
	if got := s.cliWorkspace(guest.hostPath); got != guest.guestPath {
		t.Fatalf("Claude cwd=%q", got)
	}
	for _, route := range []string{"/api/browser/editor", "/api/macos-browser/action"} {
		fields := map[string]string{"workspace": guest.hostPath}
		if route == "/api/macos-browser/action" {
			fields["action"] = "editor"
		}
		input, _ := json.Marshal(fields)
		response = windowsBrowserRequestForTest(s, http.MethodPost, route, input)
		if response.Code != 200 {
			t.Fatalf("default editor %s: %d %s", route, response.Code, response.Body.String())
		}
		if route == "/api/browser/editor" && !strings.Contains(response.Body.String(), path.Base(guest.guestPath)) {
			t.Fatal("editor lost shared project identity")
		}
	}
	terminal, err := s.createTerminal(terminalRequest{Workspace: guest.hostPath, Kind: "shell"})
	if err != nil {
		t.Fatal(err)
	}
	sub := terminal.subscribe()
	defer sub.Close()
	readTerminalUntil(t, sub, "shared-workspace-ready")
	probe := filepath.Join(guest.hostPath, ".cage-share-probe-boundary")
	if err = os.WriteFile(probe, []byte("fixture-boundary"), 0600); err != nil {
		t.Fatal(err)
	}
	if !s.checkMacOSSharedFile(t.Context(), probe, "fixture-boundary") {
		t.Fatal("default workspace failed shared-file round trip")
	}
	got, err := os.ReadFile(marker)
	if err != nil || string(got) != "original-host-data" {
		t.Fatal("existing host data changed")
	}
}

func TestMacOSDefaultWorkspaceRestoresWithVisibleFailureAndRetry(t *testing.T) {
	guest := newMacWorkspaceGuest(t)
	s := guest.s
	s.restoreProjectShares()
	if len(s.projectErrors) != 1 || s.projectErrors[0].Path != guest.hostPath || s.projectShares[guest.hostPath] == nil {
		t.Fatal("default workspace restoration omitted its pending authorization")
	}
	guest.approved.Store(true)
	if _, err := s.connectProject(guest.hostPath, false); err != nil {
		t.Fatal(err)
	}
	if len(s.projectErrors) != 0 {
		t.Fatal("successful retry retained the old project error")
	}
}

func TestMacOSDefaultWorkspaceNewAndLegacyConversations(t *testing.T) {
	s := testServer(t)
	s.macosGuest = &guestos.MacOSProfile{Instance: "mac", User: "cage"}
	host, _ := filepath.EvalSymlinks(t.TempDir())
	if err := writePrivate(filepath.Join(s.profile, "environment.json"), map[string]string{"directory": host}); err != nil {
		t.Fatal(err)
	}
	s.preferences.Workspace = host
	script, fresh, err := s.prepareTerminalConversation(host, claudeSelection{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.GuestWorkspace != macOSProjectPath("cage", host) || !strings.Contains(script, "cage-macos-terminal --claude") || fresh.ConnectionNotice != "" {
		t.Fatal("new default conversation did not use shared project")
	}
	legacyRoot := "/Users/cage/.local/share/cage/workspace"
	for _, directory := range []string{legacyRoot, legacyRoot + "/nested"} {
		id := uuid.NewString()
		if _, err = s.mobileCLI.create(s.profile, host, id); err != nil {
			t.Fatal(err)
		}
		record, err := s.mobileCLI.record(s.profile, id)
		if err != nil {
			t.Fatal(err)
		}
		record.Terminal, record.Resume, record.GuestWorkspace = true, true, directory
		if err = writePrivate(mobileRecordPath(s.profile, id), record); err != nil {
			t.Fatal(err)
		}
		script, resumed, err := s.prepareTerminalConversation(host, claudeSelection{}, id)
		if err != nil {
			t.Fatal(err)
		}
		if resumed.GuestWorkspace != directory || resumed.ConnectionNotice != macOSLegacyWorkspaceNotice || strings.Contains(script, "cage-macos-terminal") || !strings.Contains(script, "cd -- "+legacyRoot) {
			t.Fatal("legacy conversation lost original directory or notice")
		}
		s.terminals.sessions = map[string]*terminalSession{"legacy": {id: "legacy", sharedID: id, workspace: host}}
		if s.terminalSummaries()[0]["workspace_notice"] != macOSLegacyWorkspaceNotice {
			t.Fatal("desktop terminal omitted the legacy workspace notice")
		}
		if _, _, err = s.prepareTerminalConversation(host+"/other", claudeSelection{}, id); err == nil {
			t.Fatal("another project reused a legacy workspace binding")
		}
	}
	for _, record := range []*mobileRecord{
		{Terminal: true, GuestWorkspace: legacyRoot + "/../escape", mobileSession: mobileSession{mobileSummary: mobileSummary{Workspace: host}}},
		{Terminal: true, GuestWorkspace: legacyRoot + "-other", mobileSession: mobileSession{mobileSummary: mobileSummary{Workspace: host}}},
		{Terminal: true, GuestWorkspace: legacyRoot, mobileSession: mobileSession{mobileSummary: mobileSummary{Workspace: host + "/other"}}},
		{GuestWorkspace: legacyRoot, mobileSession: mobileSession{mobileSummary: mobileSummary{Workspace: host}}},
	} {
		if s.macOSLegacyWorkspace(record) != "" {
			t.Fatal("legacy compatibility accepted an unrelated or unbound directory")
		}
	}
}

func TestMacOSProjectOpenRestoresPendingApprovalWindow(t *testing.T) {
	var activations atomic.Int32
	var activationFails atomic.Bool
	guest := newMacWorkspaceGuest(t, func(channel ssh.Channel, command string) (uint32, bool) {
		if strings.Contains(command, "/usr/bin/open -a /Applications/CageProjectAccess.app") {
			activations.Add(1)
			if activationFails.Load() {
				return 1, true
			}
			return 0, true
		}
		return 0, false
	})
	s := guest.s
	s.restoreProjectShares()
	share := s.projectShares[guest.hostPath]
	if share == nil || share.approval == "" {
		t.Fatal("expected a pending project authorization")
	}
	grant := share.approval
	if err := s.ensureProjectShare(guest.hostPath); !errors.Is(err, errMacOSProjectApproval) {
		t.Fatalf("background project probe: %v", err)
	}
	if activations.Load() != 0 {
		t.Fatal("background project recovery stole guest focus")
	}
	body, _ := json.Marshal(map[string]any{"workspace": guest.hostPath, "add": false})
	open := func() *httptest.ResponseRecorder {
		return windowsBrowserRequestForTest(s, http.MethodPost, "/api/projects/open", body)
	}
	response := open()
	var pending struct{ Workspace, Approval string }
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &pending) != nil || pending.Approval != grant || pending.Workspace != guest.hostPath {
		t.Fatalf("pending project open: %d %s", response.Code, response.Body.String())
	}
	if activations.Load() != 1 || share.detach != nil {
		t.Fatal("explicit project open did not show the pending window without approving it")
	}
	activationFails.Store(true)
	response = open()
	if response.Code == http.StatusOK || share.approval != grant || s.projectShares[guest.hostPath] != share || share.detach != nil {
		t.Fatal("failed window activation lost its pending grant or reported success")
	}
	activationFails.Store(false)
	guest.approved.Store(true)
	response = open()
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "approval") || share.detach == nil {
		t.Fatalf("approved project open: %d %s", response.Code, response.Body.String())
	}
	if activations.Load() != 2 {
		t.Fatal("opening an approved project stole guest focus")
	}
}
