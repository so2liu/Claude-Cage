package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	agentvm "github.com/sylvinus/agent-vm"
	"github.com/sylvinus/agent-vm/internal/appupdate"
	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/cli"
	"github.com/sylvinus/agent-vm/internal/containerenv"
	"github.com/sylvinus/agent-vm/internal/diagnostics"
	"github.com/sylvinus/agent-vm/internal/guestos"
	"github.com/sylvinus/agent-vm/internal/host"
	"github.com/sylvinus/agent-vm/internal/hostproxy"
	"github.com/sylvinus/agent-vm/internal/limaembed"
	"github.com/sylvinus/agent-vm/internal/managedproxy"
	"github.com/sylvinus/agent-vm/internal/workbench"
	"io"
	"os/signal"
	"runtime/debug"
	"sync"
	"syscall"
)

const help = `Claude Cage — local isolated Claude development

cage desktop       Open the unified Cage workbench (desktop launcher)
cage agent         Discover and use the versioned, JSON-only external Agent interface
cage serve         Run its private localhost control service
cage server-entry --json  Issue a private SSH remote connection entry
cage container-configure --json  Configure a Linux container from JSON stdin
cage init          Create a private profile; add and select nodes in Cage settings
cage check         Verify proxy transport (no VM required)
cage guest-os --json  Check host prerequisites for a same-system guest
                     On Windows, add --vm-id GUID to inspect one Hyper-V VM
cage proxy-start   Start Cage's bundled proxy core with the applied node
cage proxy-stop    Stop Cage's core and close external networking
cage setup         Build the isolated base VM through the specified proxy
cage up            Start the shared Cage environment, editor and browser
cage login         Start cage services, then Claude's login flow inside the VM
cage access        Inspect legacy viewer credentials (diagnostics only)
cage shell         Open a shell in the shared environment
cage run COMMAND   Execute a command in the shared environment
cage claude        Run Claude Code in the shared environment
cage stop          Stop the shared Cage environment
cage list          List this cage profile's VMs
cage doctor        Diagnose the VM
cage diagnostics --json PATH.zip   Export a redacted diagnostic bundle
cage version       Print this build's version. --min VERSION fails if older
cage licenses      Print third-party license notices

Configuration: ~/.claude-cage/config.json (override CLAUDE_CAGE_CONFIG).
Each configuration directory is an independent cage profile.
Only explicitly opened project folders are shared. No host Claude credentials are imported.
`

func main() {
	defer func() {
		if failure := recover(); failure != nil {
			diagnostics.Log("error", "command", fmt.Sprintf("panic: %v\n%s", failure, debug.Stack()))
			panic(failure)
		}
	}()
	code := run()
	if code != 0 {
		diagnostics.Log("error", "command", "operation exited unsuccessfully")
	}
	os.Exit(code)
}
func run() int {
	hostproxy.CaptureEnvironment()
	// Apply this before desktop/serve dispatch: the control service and every
	// child process must keep Cage's isolation even when launched from a shell
	// configured to relax agent-vm's protections.
	for _, name := range []string{
		"AGENT_VM_UNSAFE_OPEN_NETWORK",
		"AGENT_VM_UNSAFE_WRITABLE_GIT",
		"AGENT_VM_UNSAFE_DISABLE_SECURITY_PROMPTS",
	} {
		os.Unsetenv(name)
	}
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "host-download" {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		if err := hostDownload(ctx, args[1:], os.Stdout); err != nil {
			return fail(err)
		}
		return 0
	}
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		fmt.Print(help)
		return 0
	}
	// These read only this build's own metadata. Answering them before the
	// profile is opened keeps `cage version --min` usable on a fresh install.
	if args[0] == "version" || args[0] == "--version" || args[0] == "-V" || args[0] == "licenses" {
		return cli.Main(args, cli.Stdio())
	}
	if args[0] == "agent" {
		return runAgent(args[1:], os.Stdin, os.Stdout)
	}
	if args[0] == "verify-web-assets" {
		if len(args) != 1 {
			return 2
		}
		if err := workbench.VerifyWebAssets(); err != nil {
			return fail(err)
		}
		return 0
	}
	if len(args) == 1 && (args[0] == "verify-macos-guest" || args[0] == "verify-macos-trial") {
		if !macGuestLoginAvailable() {
			return fail(fmt.Errorf("this helper was built without the virtual Mac login view"))
		}
		return 0
	}
	macGuestCommand := len(args) >= 2 && args[0] == "guest-os" && args[1] == "macos"
	if args[0] == "guest-os" && !macGuestCommand {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := writeGuestOSInspection(ctx, args[1:], os.Stdout); err != nil {
			return fail(err)
		}
		return 0
	}
	if args[0] == "verify-vm-assets" {
		if len(args) != 1 {
			return 2
		}
		dir, err := os.MkdirTemp("", "cage-vm-assets-")
		if err != nil {
			return fail(err)
		}
		defer os.RemoveAll(dir)
		share, err := limaembed.WriteShare(dir)
		if err != nil {
			return fail(err)
		}
		if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
			if _, err := share.GuestAgent("Darwin", "aarch64"); err != nil {
				return fail(err)
			}
		}
		return 0
	}
	if err := host.ConfigureWindowsRuntime(); err != nil {
		return fail(err)
	}
	path := os.Getenv(cage.ConfigEnv)
	if path == "" {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, ".claude-cage", "config.json")
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return fail(err)
	}
	store := diagnostics.New(filepath.Dir(path))
	diagnostics.SetDefault(store)
	if args[0] == "windows-network-host" {
		if len(args) != 1 {
			return fail(fmt.Errorf("windows-network-host takes its bound configuration from the profile"))
		}
		if err := workbench.ServeWindowsPacketNetwork(path); err != nil {
			return fail(err)
		}
		return 0
	}
	if args[0] == "container-runtime" {
		if len(args) != 2 {
			return fail(fmt.Errorf("container-runtime prepare|start|stop|inspect"))
		}
		environment, err := containerenv.Load(filepath.Dir(path))
		if err != nil {
			return fail(err)
		}
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		switch args[1] {
		case "prepare":
			err = environment.Prepare(ctx, os.Stdout)
		case "start":
			err = environment.Start(ctx, path, os.Stdout)
		case "stop":
			err = environment.Stop(ctx)
		case "inspect":
			inspection, inspectErr := environment.Inspect(ctx)
			err = inspectErr
			if err == nil {
				err = json.NewEncoder(os.Stdout).Encode(inspection)
			}
		default:
			err = fmt.Errorf("container-runtime prepare|start|stop|inspect")
		}
		if err != nil {
			return fail(err)
		}
		return 0
	}
	if args[0] == "container-configure" {
		if len(args) != 2 || args[1] != "--json" {
			return fail(fmt.Errorf("container-configure --json < configuration.json"))
		}
		if err := configureContainer(path); err != nil {
			return fail(err)
		}
		return 0
	}
	if args[0] == "container-install-script" {
		if len(args) != 2 {
			return fail(fmt.Errorf("container-install-script tools|services"))
		}
		script, err := containerInstallScript(args[1])
		if err != nil {
			return fail(err)
		}
		_, err = io.WriteString(os.Stdout, script)
		if err != nil {
			return 1
		}
		return 0
	}
	if args[0] == "container-network" {
		if len(args) != 1 {
			return 2
		}
		environment, err := containerenv.Load(filepath.Dir(path))
		if err != nil {
			return fail(err)
		}
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		if err := environment.ServeNetwork(ctx, path); err != nil {
			return fail(err)
		}
		return 0
	}
	if args[0] == "container-tap" {
		if len(args) != 1 {
			return 2
		}
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		if err := containerenv.RunTAP(ctx, os.Stdin, os.Stdout); err != nil {
			return fail(err)
		}
		return 0
	}
	if args[0] == "server-entry" {
		if len(args) != 2 || args[1] != "--json" {
			return fail(fmt.Errorf("server-entry --json"))
		}
		entry, err := workbench.ServerEntry(path)
		if err != nil {
			return fail(err)
		}
		if err := json.NewEncoder(os.Stdout).Encode(entry); err != nil {
			return 1
		}
		return 0
	}
	if args[0] == "diagnostic-event" {
		var event struct {
			Source  string `json:"source"`
			Message string `json:"message"`
		}
		if json.NewDecoder(io.LimitReader(os.Stdin, 32<<10)).Decode(&event) != nil {
			return 2
		}
		if event.Source != "native" {
			return 2
		}
		if err := store.Record("error", event.Source, event.Message); err != nil {
			return 1
		}
		return 0
	}
	if args[0] == "diagnostics" {
		connection := ""
		if len(args) == 5 && args[1] == "--json" && args[2] == "--connection" {
			connection = args[3]
			args = []string{args[0], args[1], args[4]}
		}
		if len(args) != 3 || args[1] != "--json" {
			return fail(fmt.Errorf("diagnostics --json /absolute/path.zip"))
		}
		if err := workbench.ExportDiagnosticsForConnection(workbench.DesktopHelperConfig(path, connection), connection, args[2]); err != nil {
			diagnostics.Log("error", "command", err.Error())
			_ = json.NewEncoder(os.Stdout).Encode(map[string]string{"status": "error", "message": err.Error()})
			return 1
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]string{"status": "ok"})
		return 0
	}
	if args[0] == "log-relay" {
		if len(args) != 2 {
			return 2
		}
		return relay(path, args[1])
	}
	if args[0] == "app-update" {
		return appupdate.Run(path, args[1:], os.Stdout)
	}
	connectionID := ""
	if (args[0] == "clipboard-files" || args[0] == "upload-files" || args[0] == "browser-file" || args[0] == "desktop-file" || args[0] == "share-project") && len(args) >= 3 && args[2] == "--connection" {
		if len(args) < 4 || args[3] == "" {
			return fail(fmt.Errorf("missing file connection identity"))
		}
		connectionID = args[3]
		args = append(append([]string{}, args[:2]...), args[4:]...)
	}
	if args[0] == "clipboard-files" {
		if len(args) < 4 || args[1] != "--json" {
			return fail(fmt.Errorf("clipboard-files --json --connection ID send|receive PATH..."))
		}
		result, err := workbench.ClipboardFilesForConnection(workbench.DesktopHelperConfig(path, connectionID), connectionID, args[2], args[3:])
		if err != nil {
			_ = json.NewEncoder(os.Stdout).Encode(map[string]string{"message": err.Error()})
			return 1
		}
		_ = json.NewEncoder(os.Stdout).Encode(result)
		return 0
	}
	if args[0] == "desktop-file" {
		if len(args) != 3 || args[1] != "--json" {
			return fail(fmt.Errorf("desktop-file --json --connection ID REQUEST"))
		}
		result, err := workbench.DesktopFileForConnection(workbench.DesktopHelperConfig(path, connectionID), connectionID, args[2])
		if err != nil {
			_ = json.NewEncoder(os.Stdout).Encode(map[string]string{"status": "error", "message": err.Error()})
			return 1
		}
		_ = json.NewEncoder(os.Stdout).Encode(result)
		return 0
	}
	if args[0] == "share-project" {
		if connectionID == "" || len(args) != 4 || args[1] != "--json" || (args[2] != "read" && args[2] != "write") {
			return fail(fmt.Errorf("share-project --json --connection ID read|write DIRECTORY"))
		}
		result, err := workbench.ShareNativeCloudProject(workbench.DesktopHelperConfig(path, connectionID), connectionID, args[3], args[2] == "read")
		if err != nil {
			_ = json.NewEncoder(os.Stdout).Encode(map[string]string{"status": "error", "message": err.Error()})
			return 1
		}
		_ = json.NewEncoder(os.Stdout).Encode(result)
		return 0
	}
	if args[0] == "upload-files" {
		if len(args) < 5 || args[1] != "--json" {
			return fail(fmt.Errorf("upload-files --json browser|desktop|terminal ID PATH..."))
		}
		result, err := workbench.UploadHostFilesForConnection(workbench.DesktopHelperConfig(path, connectionID), connectionID, args[2], args[3], args[4:])
		if err != nil {
			diagnostics.Log("error", "command", err.Error())
			_ = json.NewEncoder(os.Stdout).Encode(map[string]string{"status": "error", "message": err.Error()})
			return 1
		}
		_ = json.NewEncoder(os.Stdout).Encode(result)
		return 0
	}
	if args[0] == "browser-file" {
		if len(args) < 4 || len(args) > 5 || args[1] != "--json" {
			return fail(fmt.Errorf("browser-file --json save|open|reveal ID [destination]"))
		}
		destination := ""
		if len(args) == 5 {
			destination = args[4]
		}
		result, err := workbench.BrowserFileForConnection(workbench.DesktopHelperConfig(path, connectionID), connectionID, args[2], args[3], destination)
		if err != nil {
			diagnostics.Log("error", "command", err.Error())
			_ = json.NewEncoder(os.Stdout).Encode(map[string]string{"status": "error", "message": err.Error()})
			return 1
		}
		_ = json.NewEncoder(os.Stdout).Encode(result)
		return 0
	}
	if args[0] == "serve-desktop" {
		if err := workbench.ServeDesktop(path); err != nil {
			return fail(err)
		}
		return 0
	}
	if args[0] == "serve" {
		if err := workbench.Serve(path); err != nil {
			return fail(err)
		}
		return 0
	}
	if args[0] == "desktop" {
		options := workbench.DesktopOptions{}
		returnJSON := false
		for _, argument := range args[1:] {
			switch argument {
			case "--json":
				returnJSON = true
			case "--complete-update":
				options.CompleteUpdate = true
			case "--existing":
				options.UseExisting = true
			default:
				return fail(fmt.Errorf("unknown desktop option: %s", argument))
			}
		}
		entry, err := workbench.DesktopEntry(path, options)
		if err != nil {
			if returnJSON {
				diagnostics.Log("error", "command", err.Error())
				_ = json.NewEncoder(os.Stdout).Encode(map[string]string{"status": "error", "message": err.Error()})
				return 1
			}
			return fail(err)
		}
		// Native launchers consume this on a pipe, never in a shared log.
		if returnJSON {
			_ = json.NewEncoder(os.Stdout).Encode(entry)
		} else {
			if entry.URL == "" {
				return fail(fmt.Errorf("%s", entry.Message))
			}
			if err := openWorkbench(entry.URL); err != nil {
				return fail(err)
			}
			fmt.Println("Cage workbench opened.")
		}
		return 0
	}
	if args[0] == "prepare-update" {
		stop, returnJSON := false, false
		for _, argument := range args[1:] {
			switch argument {
			case "--stop":
				stop = true
			case "--json":
				returnJSON = true
			default:
				return fail(fmt.Errorf("unknown prepare-update option: %s", argument))
			}
		}
		result, err := workbench.PrepareDesktopUpdate(path, stop)
		if err != nil {
			if returnJSON {
				diagnostics.Log("error", "command", err.Error())
				_ = json.NewEncoder(os.Stdout).Encode(map[string]string{"status": "error", "message": err.Error()})
				return 1
			}
			return fail(err)
		}
		if returnJSON {
			_ = json.NewEncoder(os.Stdout).Encode(result)
		} else if result.Status != "prepared" {
			return fail(fmt.Errorf("%s", result.Message))
		}
		return 0
	}
	if args[0] == "init" {
		if err = host.EnsurePrivateDir(filepath.Dir(path)); err != nil {
			return fail(err)
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return fail(err)
		}
		enc := json.NewEncoder(f)
		enc.SetIndent("", "  ")
		err = enc.Encode(cage.DefaultConfig())
		f.Close()
		if err != nil {
			return fail(err)
		}
		fmt.Println("Configuration created:", path)
		return 0
	}
	cfg, err := cage.Load(path)
	management := args[0] == "stop" || args[0] == "list" || args[0] == "info"
	if err != nil && !management {
		return fail(err)
	}

	os.Setenv(cage.ConfigEnv, path)
	profile := filepath.Dir(path)
	state := filepath.Join(profile, "state")
	os.Unsetenv("CAGE_LIMA_CACHE_DIR")
	os.Unsetenv("CAGE_LOGICAL_LIMA_HOME")
	if !containerenv.Configured(profile) {
		if err = guestos.RecoverWindowsStorage(context.Background(), profile); err != nil {
			return fail(err)
		}
		lease, location, err := cage.AcquireStorageUse(profile)
		if err != nil {
			return fail(err)
		}
		defer lease.Close()
		// Keep image caches on the same volume the initialization page shows,
		// including the default location. The saved path survives app updates.
		os.Setenv("CAGE_LIMA_CACHE_DIR", filepath.Join(location.Directory, "cache", "lima"))
		if location.Custom {
			// Resolve the profile prefix once, as Lima did before migration.
			// Keep state itself logical so its original disk and socket paths survive.
			realProfile, err := filepath.EvalSymlinks(profile)
			if err != nil {
				return fail(err)
			}
			state = filepath.Join(realProfile, "state")
			os.Setenv("CAGE_LOGICAL_LIMA_HOME", filepath.Join(state, "lima"))
		}
	}
	os.Setenv("AGENT_VM_STATE_DIR", state)
	os.Setenv("AGENT_VM_LIMA_HOME", filepath.Join(state, "lima"))
	// Do not inherit host proxy settings or credentials into VM operations.
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "FTP_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "all_proxy", "ftp_proxy", "no_proxy", "ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"} {
		os.Unsetenv(name)
	}
	// Lima's host-side image downloader follows system proxy settings/TUN.
	// Guest traffic keeps the isolated hostagent hooks in limaembed.Run.
	http.DefaultTransport = hostproxy.NewTransport()
	if limaembed.Run(args) {
		return 0
	}
	if macGuestCommand {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		if err := runMacGuest(ctx, profile, state, args[2:]); err != nil {
			return fail(err)
		}
		return 0
	}
	if args[0] == "proxy-start" || args[0] == "proxy-stop" {
		if err := proxyCommand(args[0], filepath.Dir(path), cfg); err != nil {
			return fail(err)
		}
		return 0
	}
	if handled, err := containerOperation(path, args, cfg); handled {
		if err != nil {
			return fail(err)
		}
		return 0
	}
	// Every Cage operation targets this profile's one persistent environment.
	switch args[0] {
	case "up", "login", "shell", "run", "claude", "stop", "doctor", "access", "name":
		directory := cage.EnvironmentDir(filepath.Dir(path))
		if err := os.MkdirAll(directory, 0700); err != nil {
			return fail(err)
		}
		if err := os.Chdir(directory); err != nil {
			return fail(err)
		}
	}
	if management {
		code := cli.Main(args, cli.Stdio())
		if code == 0 && args[0] == "stop" {
			if err := managedproxy.Stop(filepath.Dir(path)); err != nil {
				return fail(err)
			}
		}
		return code
	}
	if err := cfg.RequireManagedProxy(filepath.Dir(path)); err != nil {
		return fail(err)
	}
	if err := cfg.CheckRunning(state); err != nil {
		return fail(err)
	}

	if args[0] == "check" {
		e, err := cage.NewEgress(cfg)
		if err != nil {
			return fail(err)
		}
		client := &http.Client{Timeout: 25 * time.Second, Transport: &http.Transport{DialContext: e.DialContext, Proxy: nil}}
		req, _ := http.NewRequestWithContext(context.Background(), "GET", "https://1.1.1.1/cdn-cgi/trace", nil)
		r, err := client.Do(req)
		if err != nil {
			return fail(fmt.Errorf("proxy transport check failed (no direct fallback)"))
		}
		r.Body.Close()
		if r.StatusCode != 200 {
			return fail(fmt.Errorf("proxy check returned HTTP %d", r.StatusCode))
		}
		fmt.Println("Proxy transport OK. Guest traffic still requires VM acceptance checks.")
		return 0
	}
	if args[0] == "setup" {
		if err = os.MkdirAll(state, 0700); err != nil {
			return fail(err)
		}
		if err = os.WriteFile(filepath.Join(state, "setup.sh"), []byte(cage.GuestSetup), 0600); err != nil {
			return fail(err)
		}
		memory, cpus, resourceErr := cage.ConfiguredResourceLimits(path)
		if resourceErr != nil {
			return fail(resourceErr)
		}
		args = append([]string{"setup", "--memory", fmt.Sprint(memory), "--cpus", fmt.Sprint(cpus), "--disk", "24", "--preinstall=node,python,chromium,gh,claude,code-server"}, args[1:]...)
	}
	if args[0] == "up" || args[0] == "login" {
		result := cli.Main([]string{"run", "true"}, cli.Stdio())
		if result != 0 {
			return result
		}
		if err := workbench.StartGuestServices(context.Background(), path, cfg, os.Stdout, os.Stderr); err != nil {
			return fail(err)
		}
		fmt.Println("Cage local services are ready. Open Cage or run cage desktop to complete the required network checks. External networking stays closed until the workbench approves this VM; no local access password is required.")
		if args[0] == "login" {
			return cli.Main([]string{"run", "--tty", "bash", "-lc", "source /etc/profile.d/claude-cage.sh; exec claude auth login"}, cli.Stdio())
		}
		return 0
	}
	if args[0] == "update-tools" {
		if result := cli.Main([]string{"run", "true"}, cli.Stdio()); result != 0 {
			return result
		}
		if err := workbench.RunMaintenanceScript(context.Background(), path, cage.ToolsUpdateScript(), os.Stdout, os.Stderr); err != nil {
			return fail(err)
		}
		return 0
	}
	if args[0] == "access" {
		args = []string{"run", "bash", "-c", `printf 'Editor password: '; sed -n 's/^password: //p' "$HOME/.config/claude-cage/code-server.yaml"; printf 'Browser password: '; cat "$HOME/.config/claude-cage/browser-password"; printf '\n'`}
	}
	allowed := map[string]bool{"setup": true, "run": true, "shell": true, "claude": true, "stop": true, "list": true, "doctor": true, "info": true, "name": true}
	if !allowed[args[0]] {
		return fail(fmt.Errorf("unknown cage command %q; see cage help", args[0]))
	}
	return cli.Main(args, cli.Stdio())
}

func containerInstallScript(kind string) (string, error) {
	switch kind {
	case "services":
		return cage.GuestSetup, nil
	case "tools":
		script := "set -e\nexport CLAUDE_CAGE=1\n" + cage.APTBootstrapScript + "\n" + cage.ToolInstallPrelude()
		enabled := map[string]bool{"NODE": true, "PYTHON": true, "CHROMIUM": true, "GH": true, "CLAUDE": true, "CODE_SERVER": true}
		for _, name := range []string{"PYTHON", "NODE", "RUBY", "RUST", "GOLANG", "DOCKER", "CHROMIUM", "GH", "CLAUDE", "OPENCODE", "CODEX", "VIBE", "PI", "CODE_SERVER", "CODE_CLAUDE", "CODE_OPENCODE", "CODE_CODEX", "CODE_VIBE", "MCP_CHROME", "MCP_PLAYWRIGHT"} {
			value := "0"
			if enabled[name] {
				value = "1"
			}
			script += "export AGENT_VM_INSTALL_" + name + "=" + value + "\n"
		}
		return script + agentvm.SetupScript, nil
	default:
		return "", fmt.Errorf("container-install-script tools|services")
	}
}

// Service assets exceed Windows' command-line limit. Give only this internal
// invocation a script stream; interactive login and ordinary run keep stdin.
func runGuestServiceStart(script string, stdio cli.IO, invoke func([]string, cli.IO) int) int {
	stdio.Stdin = strings.NewReader(script)
	return invoke([]string{"run", "bash", "-s"}, stdio)
}

func fail(err error) int {
	diagnostics.Log("error", "command", err.Error())
	fmt.Fprintln(os.Stderr, strings.TrimSpace(err.Error()))
	return 1
}

func openWorkbench(address string) error {
	if runtime.GOOS == "windows" {
		return exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", address).Start()
	}
	command := "xdg-open"
	if runtime.GOOS == "darwin" {
		command = "open"
	}
	return exec.Command(command, address).Run()
}

// Relay owns stdout/stderr capture outside the child so a Go runtime panic or
// abrupt child exit is recorded without ever persisting raw output.
func relay(configPath, kind string) int {
	var cmd *exec.Cmd
	source := "controller"
	if kind == "controller" {
		exe, err := os.Executable()
		if err != nil {
			return fail(err)
		}
		cmd = exec.Command(exe, "serve")
	} else if kind == "proxy" {
		core, err := managedproxy.CorePath()
		if err != nil {
			return fail(err)
		}
		profile := filepath.Dir(configPath)
		cmd = exec.Command(core, "-d", managedproxy.Dir(profile), "-f", filepath.Join(managedproxy.Dir(profile), "config.yaml"))
		source = "proxy"
	} else {
		return 2
	}
	configureRelayCommand(cmd)
	cmd.Env = append(os.Environ(), cage.ConfigEnv+"="+configPath)
	store := diagnostics.New(filepath.Dir(configPath))
	out := store.Stream(source, "info")
	defer out.Close()
	stderr := store.Stream(source, "error")
	defer stderr.Close()
	cmd.Stdout, cmd.Stderr = out, stderr
	if err := cmd.Start(); err != nil {
		return fail(err)
	}
	diagnostics.Log("info", source, "process started")
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	done := make(chan struct{})
	var wg sync.WaitGroup
	var forwarded os.Signal
	wg.Add(1)
	go func() {
		defer wg.Done()
		select {
		case sig := <-signals:
			forwarded = sig
			_ = cmd.Process.Signal(sig)
		case <-done:
		}
	}()
	err := cmd.Wait()
	close(done)
	wg.Wait()
	if err != nil {
		if expectedRelayStop(err, forwarded) {
			diagnostics.Log("info", source, "process stopped on request")
			return 0
		}
		diagnostics.Log("error", source, "process exited: "+err.Error())
		return 1
	}
	diagnostics.Log("info", source, "process exited normally")
	return 0
}

func expectedRelayStop(err error, forwarded os.Signal) bool {
	signal, ok := forwarded.(syscall.Signal)
	if !ok {
		return false
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return false
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	return ok && status.Signaled() && status.Signal() == signal || exit.ExitCode() == 128+int(signal)
}
