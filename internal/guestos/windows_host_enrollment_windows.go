//go:build windows

package guestos

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/Microsoft/go-winio"
	"github.com/google/uuid"
	"github.com/sylvinus/agent-vm/internal/paths"
	"golang.org/x/sys/windows"
)

const enrollmentWireLimit = 16 << 10

type enrollmentProcess struct {
	handle  windows.Handle
	pid     uint32
	started uint64
	image   string
}

func enrollmentProcessFromHandle(handle windows.Handle) (enrollmentProcess, error) {
	var value enrollmentProcess
	pid, err := windows.GetProcessId(handle)
	if err != nil || pid == 0 {
		return value, errors.New("enrollment_process_invalid")
	}
	var created, exited, kernel, user windows.Filetime
	if windows.GetProcessTimes(handle, &created, &exited, &kernel, &user) != nil {
		return value, errors.New("enrollment_process_invalid")
	}
	buffer := make([]uint16, 32768)
	size := uint32(len(buffer))
	if windows.QueryFullProcessImageName(handle, 0, &buffer[0], &size) != nil || size == 0 || size >= uint32(len(buffer)) {
		return value, errors.New("enrollment_process_invalid")
	}
	value = enrollmentProcess{handle: handle, pid: pid, started: uint64(created.HighDateTime)<<32 | uint64(created.LowDateTime), image: windows.UTF16ToString(buffer[:size])}
	return value, nil
}

func (process enrollmentProcess) alive() bool {
	if process.handle == 0 {
		return false
	}
	state, err := windows.WaitForSingleObject(process.handle, 0)
	if err != nil || state != uint32(windows.WAIT_TIMEOUT) {
		return false
	}
	fresh, err := enrollmentProcessFromHandle(process.handle)
	return err == nil && fresh.pid == process.pid && fresh.started == process.started && strings.EqualFold(fresh.image, process.image)
}

type windowsHostEnrollment struct {
	options         WindowsHostEnrollmentOptions
	installation    Installation
	instance, stage string
	owner           enrollmentProcess
	admin           enrollmentProcess
	guestSID        string
	finalized       bool
	mu              sync.Mutex
}

func newWindowsHostEnrollment(options WindowsHostEnrollmentOptions) (*windowsHostEnrollment, error) {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil || token.IsElevated() || user.User.Sid.String() != options.HostSID || options.ParentPID == 0 || options.VMID == uuid.Nil || options.InstallationID == uuid.Nil ||
		!filepath.IsAbs(options.Profile) || !filepath.IsAbs(options.State) || strings.ContainsAny(options.Profile+options.State, "\x00\r\n") ||
		!(strings.HasPrefix(options.HostSID, "S-1-5-21-") || strings.HasPrefix(options.HostSID, "S-1-12-1-")) ||
		!strings.HasPrefix(options.Pipe, "Cage.Enroll.") || len(options.Pipe) > 180 || strings.ContainsAny(options.Pipe, "\\/:\x00\r\n") {
		return nil, errors.New("enrollment_original_user_required")
	}
	var basic struct{ ExitStatus, PEB, Affinity, Priority, PID, Parent uintptr }
	var returned uint32
	if windows.NtQueryInformationProcess(windows.CurrentProcess(), windows.ProcessBasicInformation, unsafe.Pointer(&basic), uint32(unsafe.Sizeof(basic)), &returned) != nil || uint32(basic.Parent) != options.ParentPID {
		return nil, errors.New("enrollment_parent_invalid")
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, options.ParentPID)
	if err != nil {
		return nil, errors.New("enrollment_parent_invalid")
	}
	owner, err := enrollmentProcessFromHandle(handle)
	if err != nil {
		windows.CloseHandle(handle)
		return nil, err
	}
	var parentToken windows.Token
	if windows.OpenProcessToken(handle, windows.TOKEN_QUERY, &parentToken) != nil {
		windows.CloseHandle(handle)
		return nil, errors.New("enrollment_parent_invalid")
	}
	parentUser, err := parentToken.GetTokenUser()
	parentToken.Close()
	system, systemErr := windows.GetSystemDirectory()
	expected := filepath.Join(system, "WindowsPowerShell", "v1.0", "powershell.exe")
	if err != nil || parentUser.User.Sid.String() != options.HostSID || systemErr != nil || !strings.EqualFold(owner.image, expected) || !owner.alive() {
		windows.CloseHandle(handle)
		return nil, errors.New("enrollment_parent_invalid")
	}
	profile, err := paths.Resolve(options.Profile)
	if err != nil {
		windows.CloseHandle(handle)
		return nil, errors.New("enrollment_profile_invalid")
	}
	state, err := paths.Resolve(options.State)
	if err != nil {
		windows.CloseHandle(handle)
		return nil, errors.New("enrollment_profile_invalid")
	}
	anchor, err := paths.Resolve(filepath.Join(profile, "state"))
	if err != nil || !strings.EqualFold(anchor, state) {
		windows.CloseHandle(handle)
		return nil, errors.New("enrollment_profile_invalid")
	}
	options.Profile, options.State = profile, state
	installation, err := LoadInstallation(profile)
	if err != nil || installation == nil || installation.System != Windows11 || installation.ID != options.InstallationID {
		windows.CloseHandle(handle)
		return nil, errors.New("enrollment_installation_changed")
	}
	instance := filepath.Join(state, "windows-"+strings.ReplaceAll(options.InstallationID.String(), "-", ""))
	value := &windowsHostEnrollment{options: options, installation: *installation, instance: instance, stage: filepath.Join(instance, "cage-workbench", "pair"), owner: owner}
	if err = value.assertScope(); err != nil {
		windows.CloseHandle(handle)
		return nil, err
	}
	if _, err = os.Lstat(WindowsProfilePath(profile)); !errors.Is(err, os.ErrNotExist) {
		windows.CloseHandle(handle)
		return nil, errors.New("enrollment_existing_profile_preserved")
	}
	if binding, err := LoadWindowsProfile(value.stage); err != nil {
		windows.CloseHandle(handle)
		return nil, errors.New("enrollment_existing_pairing_preserved")
	} else if binding != nil {
		if err = value.bindingValid(*binding); err != nil {
			windows.CloseHandle(handle)
			return nil, err
		}
		value.guestSID = binding.GUIUserSID
	}
	return value, nil
}

func (value *windowsHostEnrollment) assertScope() error {
	if !value.owner.alive() {
		return errors.New("enrollment_parent_exited")
	}
	current, err := LoadInstallation(value.options.Profile)
	if err != nil || current == nil || *current != value.installation {
		return errors.New("enrollment_installation_changed")
	}
	receipt, err := os.ReadFile(filepath.Join(value.options.Profile, "windows-install-vm.txt"))
	if err == nil && len(receipt) > 80 {
		return errors.New("enrollment_vm_changed")
	}
	id, parse := uuid.Parse(strings.TrimSpace(string(receipt)))
	if err != nil || parse != nil || id != value.options.VMID {
		return errors.New("enrollment_vm_changed")
	}
	state, err := paths.Resolve(filepath.Join(value.options.Profile, "state"))
	if err != nil || !strings.EqualFold(state, value.options.State) {
		return errors.New("enrollment_profile_changed")
	}
	return nil
}

func (value *windowsHostEnrollment) bindingValid(binding WindowsProfile) error {
	if binding.VMID != value.options.VMID || !strings.EqualFold(filepath.Clean(binding.InstanceDirectory), value.instance) ||
		!strings.EqualFold(filepath.Clean(binding.RelayExecutable), filepath.Join(value.stage, "Cage.GuestClipboard.exe")) ||
		!strings.EqualFold(filepath.Clean(binding.PairingFile), filepath.Join(value.stage, "pairing.dpapi")) ||
		binding.NetworkPairingFile != "" && !strings.EqualFold(filepath.Clean(binding.NetworkPairingFile), filepath.Join(value.stage, "packet.dpapi")) ||
		value.guestSID != "" && binding.GUIUserSID != value.guestSID {
		return errors.New("enrollment_binding_changed")
	}
	return nil
}

func (value *windowsHostEnrollment) authorizeAdmin(request windowsHostEnrollmentRequest) error {
	if value.admin.handle != 0 || request.PID == 0 || request.ProcessHandle == 0 || request.ProcessHandle > 0x7fffffffffffffff {
		return errors.New("enrollment_admin_invalid")
	}
	handle := windows.Handle(request.ProcessHandle)
	process, err := enrollmentProcessFromHandle(handle)
	system, systemErr := windows.GetSystemDirectory()
	if err != nil || systemErr != nil || process.pid != request.PID || !strings.EqualFold(process.image, filepath.Join(system, "WindowsPowerShell", "v1.0", "powershell.exe")) || !process.alive() {
		windows.CloseHandle(handle)
		return errors.New("enrollment_admin_invalid")
	}
	value.admin = process
	return nil
}

func cryptHostEnrollment(data []byte, vm uuid.UUID, sid string, protect bool) ([]byte, error) {
	input := make([]byte, len(data))
	copy(input, data)
	defer clear(input)
	entropy := sha256.Sum256([]byte("Cage.HyperV.v1:" + vm.String() + ":" + sid))
	in := windows.DataBlob{Size: uint32(len(input)), Data: &input[0]}
	salt := windows.DataBlob{Size: 32, Data: &entropy[0]}
	var output windows.DataBlob
	var err error
	if protect {
		err = windows.CryptProtectData(&in, nil, &salt, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &output)
	} else {
		err = windows.CryptUnprotectData(&in, nil, &salt, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &output)
	}
	runtime.KeepAlive(input)
	runtime.KeepAlive(entropy)
	if output.Data != nil {
		defer func() {
			if output.Size <= enrollmentWireLimit {
				clear(unsafe.Slice(output.Data, int(output.Size)))
			}
			windows.LocalFree(windows.Handle(unsafe.Pointer(output.Data)))
		}()
	}
	if err != nil || output.Data == nil || output.Size == 0 || output.Size > enrollmentWireLimit {
		return nil, errors.New("enrollment_current_user_crypto_failed")
	}
	result := append([]byte(nil), unsafe.Slice(output.Data, int(output.Size))...)
	return result, nil
}

func (value *windowsHostEnrollment) privateKey(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > enrollmentWireLimit {
		return nil, errors.New("enrollment_existing_pairing_preserved")
	}
	name, _ := windows.UTF16PtrFromString(path)
	attributes, err := windows.GetFileAttributes(name)
	if err != nil || attributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return nil, errors.New("enrollment_existing_pairing_preserved")
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return nil, errors.New("enrollment_existing_pairing_preserved")
	}
	owner, _, err := sd.Owner()
	control, _, controlErr := sd.Control()
	acl, _, aclErr := sd.DACL()
	if err != nil || owner == nil || owner.String() != value.options.HostSID || controlErr != nil || control&windows.SE_DACL_PROTECTED == 0 || aclErr != nil || acl == nil {
		return nil, errors.New("enrollment_existing_pairing_preserved")
	}
	for i := uint32(0); i < uint32(acl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if windows.GetAce(acl, i, &ace) != nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return nil, errors.New("enrollment_existing_pairing_preserved")
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if sid.String() != value.options.HostSID && !sid.IsWellKnown(windows.WinBuiltinAdministratorsSid) && !sid.IsWellKnown(windows.WinLocalSystemSid) {
			return nil, errors.New("enrollment_existing_pairing_preserved")
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("enrollment_existing_pairing_preserved")
	}
	return data, nil
}

func (value *windowsHostEnrollment) storeKey(ctx context.Context, kind, sid string, key []byte) error {
	if ctx.Err() != nil {
		return errors.New("enrollment_cancelled")
	}
	if len(key) != 32 || !windowsGUISID.MatchString(sid) || value.guestSID != "" && value.guestSID != sid {
		return errors.New("enrollment_guest_changed")
	}
	if err := value.assertScope(); err != nil {
		return err
	}
	if !value.admin.alive() {
		return errors.New("enrollment_admin_exited")
	}
	value.guestSID = sid
	name := "pairing.dpapi"
	if kind == "deliver-packet" {
		name = "packet.dpapi"
	}
	path := filepath.Join(value.stage, name)
	if _, err := os.Lstat(path); err == nil {
		data, err := value.privateKey(path)
		if err != nil {
			return err
		}
		old, err := cryptHostEnrollment(data, value.options.VMID, sid, false)
		if err != nil {
			return err
		}
		defer clear(old)
		if subtle.ConstantTimeCompare(old, key) != 1 {
			return errors.New("enrollment_existing_pairing_preserved")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("enrollment_existing_pairing_preserved")
	}
	stage, err := paths.Resolve(value.stage)
	if err != nil || !strings.EqualFold(stage, value.stage) {
		return errors.New("enrollment_profile_changed")
	}
	data, err := cryptHostEnrollment(key, value.options.VMID, sid, true)
	if err != nil {
		return err
	}
	sd, err := windows.SecurityDescriptorFromString("O:" + value.options.HostSID + "D:P(A;;FA;;;" + value.options.HostSID + ")(A;;FA;;;SY)(A;;FA;;;BA)")
	if err != nil {
		return errors.New("enrollment_permissions_invalid")
	}
	attributes := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	pathPtr, _ := windows.UTF16PtrFromString(path)
	if ctx.Err() != nil || !value.admin.alive() {
		return errors.New("enrollment_cancelled")
	}
	handle, err := windows.CreateFile(pathPtr, windows.FILE_WRITE_DATA|windows.READ_CONTROL|windows.WRITE_DAC|windows.DELETE, 0, &attributes, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
	runtime.KeepAlive(sd)
	if err != nil {
		return errors.New("enrollment_pairing_not_saved")
	}
	file := os.NewFile(uintptr(handle), path)
	completed := false
	defer func() {
		if !completed {
			discard := byte(1)
			_ = windows.SetFileInformationByHandle(handle, windows.FileDispositionInfo, &discard, 1)
		}
		file.Close()
	}()
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	if err != nil {
		return errors.New("enrollment_pairing_not_saved")
	}
	if err = value.assertScope(); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return errors.New("enrollment_cancelled")
	}
	if !value.admin.alive() {
		return errors.New("enrollment_admin_exited")
	}
	completed = true
	return nil
}

func (value *windowsHostEnrollment) finalize(ctx context.Context) error {
	if value.finalized {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	binding, err := LoadWindowsProfile(value.stage)
	if err != nil || binding == nil || value.bindingValid(*binding) != nil || binding.GUIUserSID != value.guestSID || binding.NetworkPairingFile == "" {
		return errors.New("enrollment_binding_not_ready")
	}
	for _, name := range []string{"pairing.dpapi", "packet.dpapi"} {
		data, err := value.privateKey(filepath.Join(value.stage, name))
		if err != nil {
			return err
		}
		key, err := cryptHostEnrollment(data, value.options.VMID, value.guestSID, false)
		if err != nil {
			return err
		}
		valid := len(key) == 32
		clear(key)
		if !valid {
			return errors.New("enrollment_binding_not_ready")
		}
	}
	var ready bool
	for ctx.Err() == nil {
		if !value.admin.alive() {
			return errors.New("enrollment_admin_exited")
		}
		if err = value.assertScope(); err != nil {
			return err
		}
		request, requestErr := WindowsRelayCommand(ctx, value.stage, "status")
		if requestErr != nil {
			return errors.New("enrollment_relay_not_ready")
		}
		var output boundedEnrollmentOutput
		request.Stdout = &output
		request.Stderr = io.Discard
		if request.Run() == nil {
			var report struct {
				Version      int      `json:"version"`
				OS           string   `json:"os"`
				VMID         string   `json:"vm_id"`
				SID          string   `json:"gui_user_sid"`
				Available    bool     `json:"available"`
				Capabilities []string `json:"capabilities"`
				Browser      struct {
					Installed       bool `json:"installed"`
					EditorInstalled bool `json:"editor_installed"`
				} `json:"browser"`
				Components map[string]struct {
					Installed bool `json:"installed"`
					Ready     bool `json:"ready"`
				} `json:"components"`
			}
			if json.Unmarshal(output.Bytes(), &report) == nil && report.Version == 1 && report.OS == "windows" && report.VMID == value.options.VMID.String() && report.SID == value.guestSID && report.Available && report.Browser.Installed && report.Browser.EditorInstalled {
				ready = true
				for _, name := range []string{"cli", "terminal", "project-mount", "desktop"} {
					component := report.Components[name]
					ready = ready && slices.Contains(report.Capabilities, name) && component.Installed && component.Ready
				}
				if ready {
					break
				}
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
	if !ready || ctx.Err() != nil || !value.admin.alive() {
		return errors.New("enrollment_components_not_ready")
	}
	if err = value.assertScope(); err != nil {
		return err
	}
	var state WindowsVMResourceState
	if err = windowsHostManagerCall(ctx, value.options.VMID, "resources", 0, 0, &state); err != nil || state.State != "Running" {
		return errors.New("enrollment_manager_not_ready")
	}
	if err = value.assertScope(); err != nil {
		return err
	}
	if !value.admin.alive() || ctx.Err() != nil {
		return errors.New("enrollment_cancelled")
	}
	if err = BindWindowsProfile(value.options.Profile, *binding); err != nil {
		return errors.New("enrollment_profile_not_published")
	}
	value.finalized = true
	return nil
}

type boundedEnrollmentOutput struct{ bytes.Buffer }

func (value *boundedEnrollmentOutput) Write(data []byte) (int, error) {
	if value.Len()+len(data) > 64<<10 {
		return 0, errors.New("enrollment_response_limit")
	}
	return value.Buffer.Write(data)
}

func (value *windowsHostEnrollment) connection(ctx context.Context, conn net.Conn, cancel context.CancelFunc) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	fd, ok := conn.(interface{ Fd() uintptr })
	var pid uint32
	if !ok || windows.GetNamedPipeClientProcessId(windows.Handle(fd.Fd()), &pid) != nil {
		return
	}
	value.mu.Lock()
	owner := pid == value.owner.pid && value.owner.alive()
	admin := pid == value.admin.pid && value.admin.alive()
	value.mu.Unlock()
	challenge := make([]byte, 32)
	if _, err := rand.Read(challenge); err != nil {
		return
	}
	defer clear(challenge)
	write := func(reply windowsHostEnrollmentReply) { reply.Version = 1; _ = json.NewEncoder(conn).Encode(reply) }
	write(windowsHostEnrollmentReply{OK: owner || admin, Authorized: owner || admin, Challenge: base64.StdEncoding.EncodeToString(challenge)})
	if !owner && !admin {
		return
	}
	reader := bufio.NewReaderSize(conn, enrollmentWireLimit+1)
	line, err := reader.ReadSlice('\n')
	if err != nil || len(line) > enrollmentWireLimit {
		return
	}
	var request windowsHostEnrollmentRequest
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil || decoder.Decode(new(any)) != io.EOF {
		return
	}
	decoded, err := base64.StdEncoding.DecodeString(request.Challenge)
	if err != nil || request.Version != 1 || request.VMID != value.options.VMID || subtle.ConstantTimeCompare(decoded, challenge) != 1 {
		return
	}
	value.mu.Lock()
	requestErr := request.validate(value.options.VMID, owner, value.guestSID)
	value.mu.Unlock()
	if requestErr != nil {
		return
	}
	key := make([]byte, 32)
	defer clear(key)
	if request.Action == "deliver-gui" || request.Action == "deliver-packet" {
		if !admin {
			return
		}
		if _, err = io.ReadFull(reader, key); err != nil {
			return
		}
	}
	if reader.Buffered() != 0 {
		return
	}
	_ = conn.SetDeadline(time.Now().Add(45 * time.Second))
	operation, finish := context.WithCancel(ctx)
	defer finish()
	disconnected := make(chan struct{})
	go func() { defer close(disconnected); var extra [1]byte; _, _ = conn.Read(extra[:]); finish() }()
	defer func() { conn.Close(); <-disconnected }()
	value.mu.Lock()
	defer value.mu.Unlock()
	if err = value.assertScope(); err != nil {
		write(windowsHostEnrollmentReply{Error: err.Error()})
		return
	}
	if owner {
		switch request.Action {
		case "ready":
		case "allow-admin":
			err = value.authorizeAdmin(request)
		case "stop":
			cancel()
		default:
			err = errors.New("enrollment_action_invalid")
		}
	} else if !value.admin.alive() {
		err = errors.New("enrollment_admin_exited")
	} else {
		switch request.Action {
		case "deliver-gui", "deliver-packet":
			err = value.storeKey(operation, request.Action, request.GUIUserSID, key)
		case "finalize":
			if request.GUIUserSID != value.guestSID {
				err = errors.New("enrollment_guest_changed")
			} else {
				err = value.finalize(operation)
			}
		default:
			err = errors.New("enrollment_action_invalid")
		}
	}
	if err != nil {
		write(windowsHostEnrollmentReply{Error: "enrollment_not_completed"})
		return
	}
	write(windowsHostEnrollmentReply{OK: true})
}

func RunWindowsHostEnrollment(parent context.Context, options WindowsHostEnrollmentOptions) error {
	value, err := newWindowsHostEnrollment(options)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(value.owner.handle)
	defer func() {
		if value.admin.handle != 0 {
			windows.CloseHandle(value.admin.handle)
		}
	}()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	// go-winio opens a second server instance after creating its placeholder.
	// The ordinary owner needs FILE_CREATE_PIPE_INSTANCE (0x4) for that open.
	listener, err := winio.ListenPipe(`\\.\pipe\`+options.Pipe, &winio.PipeConfig{SecurityDescriptor: "O:" + options.HostSID + "D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;0x12019f;;;" + options.HostSID + ")", InputBufferSize: enrollmentWireLimit, OutputBufferSize: enrollmentWireLimit})
	if err != nil {
		return enrollmentPipeError("listen", err)
	}
	defer listener.Close()
	var active sync.WaitGroup
	var connections sync.Map
	go func() {
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				listener.Close()
				connections.Range(func(key, _ any) bool { key.(net.Conn).Close(); return true })
				return
			case <-ticker.C:
				if !value.owner.alive() {
					cancel()
				}
			}
		}
	}()
	defer func() {
		cancel()
		listener.Close()
		connections.Range(func(key, _ any) bool { key.(net.Conn).Close(); return true })
		active.Wait()
	}()
	slots := make(chan struct{}, 4)
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return enrollmentPipeError("accept", err)
		}
		select {
		case slots <- struct{}{}:
		default:
			conn.Close()
			continue
		}
		connections.Store(conn, struct{}{})
		active.Add(1)
		go func() {
			defer active.Done()
			defer func() { <-slots; connections.Delete(conn) }()
			value.connection(ctx, conn, cancel)
		}()
	}
}
