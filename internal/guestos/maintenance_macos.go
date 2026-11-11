package guestos

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"time"

	"al.essio.dev/pkg/shellescape"
)

// MaintainMacOSComponents operates only on an already bound VZ identity. Its
// staging and installers are independent of account creation and OS restoration.
func MaintainMacOSComponents(ctx context.Context, profile, kit, manifestPath string, tools bool, stdout, stderr io.Writer) error {
	if runtime.GOOS != "darwin" {
		return errors.New("macOS 客机维护需要其 Mac 主机")
	}
	binding, err := LoadMacOSProfile(profile)
	if err != nil {
		return err
	}
	if binding == nil {
		return errors.New("此配置尚未绑定 macOS 客机")
	}
	required, err := RequiredNativeComponents(kit, MacOS)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return err
	}
	var selected NativeComponentManifest
	if json.Unmarshal(data, &selected) != nil || selected.Validate() != nil || !reflect.DeepEqual(required, selected) {
		return errors.New("当前应用组件包已变化，请重新发起更新")
	}
	provision, err := MacOSProvisionUser(profile)
	if err != nil {
		return err
	}
	args, err := MacOSSSHArgs(profile, provision)
	if err != nil {
		return err
	}
	guest := func(callctx context.Context, script string) *exec.Cmd {
		argv := append(append([]string{}, args...), "-T", "cage-macos", "--", "exec /bin/bash -ec "+shellescape.Quote(script))
		command := exec.CommandContext(callctx, "/usr/bin/ssh", argv...)
		command.Stderr = stderr
		return command
	}
	mode := "components"
	cache := filepath.Join(profile, "state", "guest-media", MacOS)
	if tools {
		mode = "tools"
		if err = PrepareMacOSMedia(ctx, profile, kit, cache, true, stdout, stderr); err != nil {
			return fmt.Errorf("准备原生工具包失败：%w", err)
		}
	}
	local, err := os.MkdirTemp("", "cage-native-components-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(local)
	if err = os.WriteFile(filepath.Join(local, "native-components.json"), data, 0600); err != nil {
		return err
	}
	output, err := guest(ctx, `umask 077; test "$(id -un)" = `+shellescape.Quote(provision)+`; mktemp -d /private/var/tmp/cage-components.XXXXXXXX`).Output()
	remote := strings.TrimSpace(string(output))
	if err != nil || !regexp.MustCompile(`^/private/var/tmp/cage-components\.[A-Za-z0-9]{8}$`).MatchString(remote) {
		return errors.New("无法在已绑定客机中准备组件维护目录")
	}
	passwordInput := ` < /Users/` + provision + `/password`
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = guest(cleanup, `sudo -S -p '' /bin/rm -rf `+remote+passwordInput).Run()
	}()
	archiveArgs := []string{"--no-mac-metadata", "-cf", "-", "-C", kit, ".", "-C", local, "native-components.json"}
	if tools {
		archiveArgs = append(archiveArgs, "-C", cache, "node.tgz", "editor.tgz", "playwright.tgz", "chrome.dmg", "cli")
	}
	archive := exec.CommandContext(ctx, "/usr/bin/tar", archiveArgs...)
	archive.Env = append(os.Environ(), "COPYFILE_DISABLE=1")
	var archiveErrors bytes.Buffer
	archive.Stderr = &archiveErrors
	pipe, err := archive.StdoutPipe()
	if err != nil {
		return err
	}
	receiver := guest(ctx, `umask 077; cd `+remote+`; /usr/bin/tar -xf -`)
	receiver.Stdin = pipe
	if err = archive.Start(); err != nil {
		return err
	}
	receiveErr := receiver.Run()
	pipe.Close()
	archiveErr := archive.Wait()
	if archiveErrors.Len() > 0 {
		_, _ = stderr.Write(archiveErrors.Bytes())
	}
	if err = errors.Join(receiveErr, archiveErr); err != nil {
		return fmt.Errorf("传递原生组件失败：%w", err)
	}
	takeOwnership := guest(ctx, `sudo -S -p '' /usr/sbin/chown -R root:wheel `+remote+passwordInput)
	if err = takeOwnership.Run(); err != nil {
		return errors.New("无法取得已绑定客机的组件维护权限")
	}
	script := shellescape.QuoteCommand([]string{"/bin/bash", remote + "/maintain-macos-workbench.sh", remote, binding.User, mode})
	command := guest(ctx, `sudo -S -p '' `+script+passwordInput)
	command.Stdout = stdout
	if err = command.Run(); err != nil {
		return fmt.Errorf("原生组件维护未完成，原系统和账户已保留，可重试更新：%w", err)
	}
	return nil
}

func WriteNativeComponentManifest(filename string, manifest NativeComponentManifest) error {
	if err := manifest.Validate(); err != nil {
		return err
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(filename), ".native-components-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, writeErr := file.Write(append(bytes.TrimSpace(data), '\n'))
	err = errors.Join(writeErr, file.Sync(), file.Close())
	if err != nil {
		return err
	}
	return os.Rename(file.Name(), filename)
}
