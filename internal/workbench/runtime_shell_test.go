package workbench

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"github.com/sylvinus/agent-vm/internal/cage"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestGuestScriptWithZshMiseInitialization(t *testing.T) {
	zsh, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("zsh unavailable")
	}
	if _, err := os.Stat(sshExecutable()); err != nil {
		t.Skip("OpenSSH unavailable")
	}
	home := t.TempDir()
	// Reduced from mise 2026.10.25's default zsh activation. With this
	// initialization, zsh returns 1 for a later commandless exec redirection.
	init := `[ -n "$(declare -f command_not_found_handler)" ] && true
command_not_found_handler() { return 127; }
`
	if err := os.WriteFile(filepath.Join(home, ".zshenv"), []byte(init), 0600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, script, output string
		fail                 bool
	}{
		{"resize-lock", "set -eu\nexport DISPLAY=:78\nexec 9>\"$HOME/resize.lock\"\nprintf '%s\\n' \"single ' quote\" 'literal $HOME'\n", "single ' quote\nliteral $HOME\n", false},
		{"script-failure", "printf before-failure\nexit 7\n", "", true},
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
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
	// Exercise the production SSH command and stdout handling. Only the
	// remote machine is replaced by a local server running a real zsh.
	go func() {
		for range cases {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			server, channels, requests, err := ssh.NewServerConn(conn, config)
			if err != nil {
				conn.Close()
				continue
			}
			go ssh.DiscardRequests(requests)
			for incoming := range channels {
				channel, requests, err := incoming.Accept()
				if err != nil {
					break
				}
				for request := range requests {
					if request.Type != "exec" {
						request.Reply(false, nil)
						continue
					}
					var command struct{ Command string }
					if err := ssh.Unmarshal(request.Payload, &command); err != nil {
						request.Reply(false, nil)
						break
					}
					request.Reply(true, nil)
					cmd := exec.Command(zsh, "-c", command.Command)
					cmd.Dir = home
					cmd.Env = []string{"HOME=" + home, "ZDOTDIR=" + home, "PATH=/usr/bin:/bin"}
					cmd.Stdin, cmd.Stdout, cmd.Stderr = channel, channel, channel.Stderr()
					status := uint32(0)
					if err := cmd.Run(); err != nil {
						status = 1
						var exit *exec.ExitError
						if errors.As(err, &exit) {
							status = uint32(exit.ExitCode())
						}
					}
					channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
					break
				}
				channel.Close()
				// Let the client consume exit-status and close its transport.
				// Closing the TCP connection here can turn success into SSH 255.
			}
			server.Close()
		}
	}()
	s := testServer(t)
	dir := filepath.Join(s.profile, "state", "lima", cage.InstanceName(s.profile, s.environmentDir()))
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	clientConfig := fmt.Sprintf("Host *\n HostName 127.0.0.1\n Port %d\n User cage\n StrictHostKeyChecking no\n UserKnownHostsFile %s\n", listener.Addr().(*net.TCPAddr).Port, os.DevNull)
	if err := os.WriteFile(filepath.Join(dir, "ssh.config"), []byte(clientConfig), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			out, err := s.guest(ctx, tc.script)
			if (err != nil) != tc.fail || string(out) != tc.output {
				t.Fatalf("guest returned %q, %v", out, err)
			}
		})
	}
}
