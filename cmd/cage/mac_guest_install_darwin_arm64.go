//go:build darwin && arm64 && cgo && !no_vz

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"al.essio.dev/pkg/shellescape"
	"github.com/sylvinus/agent-vm/internal/guestos"
	"github.com/sylvinus/agent-vm/internal/vm"
)

const appleImageURL = "https://updates.cdn-apple.com/2026SpringFCS/fullrestores/140-24263/B95838F0-6815-4F0B-A039-156526C081AD/UniversalMac_26.5.2_25F84_Restore.ipsw"
const appleImageDigest = "065abd295a1a456a46c1155217eab92ee95816520ec9aeed83f249f074f68a04"
const appleImageBytes int64 = 19769902281

func installMacWorkbench(ctx context.Context, profile, state string, l *vm.Lima) error {
	i, err := guestos.LoadInstallation(profile)
	if err != nil || i == nil || i.System != guestos.MacOS {
		return errors.New("缺少这台虚拟 Mac 的首次安装记录")
	}
	pw, err := io.ReadAll(io.LimitReader(os.Stdin, 257))
	if err != nil || !guestos.ValidMacOSInstallPassword(string(pw)) {
		return errors.New("客机初始密码需使用 8 至 256 个英文键盘可输入的字母、数字、空格或符号")
	}
	defer clear(pw)
	ordinaryUser := os.Getenv("CAGE_MACOS_ACCOUNT")
	if !guestos.ValidMacOSAccountName(ordinaryUser) {
		return errors.New("请从 Cage 注册共用账密后继续安装 macOS 客机")
	}
	if !macGuestLoginAvailable() {
		return errors.New("此 Cage 构建缺少虚拟 Mac 登录组件")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	kit := filepath.Join(filepath.Dir(exe), "guest-os")
	components, err := guestos.RequiredNativeComponents(kit, guestos.MacOS)
	if err != nil {
		return err
	}
	componentStage, err := os.MkdirTemp("", "cage-install-components-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(componentStage)
	if err = guestos.WriteNativeComponentManifest(filepath.Join(componentStage, "native-components.json"), components); err != nil {
		return err
	}
	cache := filepath.Join(state, "guest-media", i.System)
	if err := os.MkdirAll(cache, 0700); err != nil {
		return err
	}
	media := i.Media
	if media == "" {
		media = filepath.Join(cache, "MacOS.ipsw")
		fmt.Fprintln(os.Stdout, "正在从 Apple 下载并校验 macOS 26.5.2（19.77 GB）…")
		err = prepareMacRestoreMedia(ctx, media, appleImageURL, appleImageDigest, appleImageBytes, os.Stdout)
		if err != nil {
			return fmt.Errorf("Apple 恢复镜像下载失败：%w", err)
		}
	}
	fmt.Fprintln(os.Stdout, "正在准备客机应用和开发工具…")
	fmt.Fprintln(os.Stdout, "CAGE_GUEST_STAGE macos media")
	if err := guestos.PrepareMacOSMedia(ctx, profile, kit, cache, false, os.Stdout, os.Stderr); err != nil {
		return fmt.Errorf("准备客机应用失败：%w", err)
	}
	fmt.Fprintln(os.Stdout, "正在恢复虚拟 Mac…")
	fmt.Fprintln(os.Stdout, "CAGE_GUEST_STAGE macos restore")
	if err := runMacGuest(ctx, profile, state, []string{"create", media}); err != nil {
		return err
	}
	if err := l.CheckMacOSGuest(ctx, "mac"); err != nil {
		return err
	}
	if err := guestos.PinMacOSInstallHostKey(profile); err != nil {
		return err
	}
	provision, err := guestos.MacOSProvisionUser(profile)
	if err != nil {
		return err
	}
	if provision != "cageprovision" {
		return errors.New("此开发客机的置备身份不匹配")
	}
	adminPassword := " < /Users/" + provision + "/password"
	args, err := guestos.MacOSSSHArgs(profile, provision)
	if err != nil {
		return err
	}
	guestCommand := func(opctx context.Context, script string) *exec.Cmd {
		argv := append(append([]string{}, args...), "-T", "cage-macos", "--", "exec /bin/sh -ec "+shellescape.Quote(script))
		cmd := exec.CommandContext(opctx, "/usr/bin/ssh", argv...)
		cmd.Stderr = os.Stderr
		return cmd
	}
	out, err := guestCommand(ctx, `umask 077; test "$(uname -s)" = Darwin; test "$(id -un)" = `+provision+`; mktemp -d /private/var/tmp/cage-install.XXXXXXXX`).Output()
	remote := strings.TrimSpace(string(out))
	if err != nil || !regexp.MustCompile(`^/private/var/tmp/cage-install\.[A-Za-z0-9]{8}$`).MatchString(remote) {
		return errors.New("无法在已验证的虚拟 Mac 中准备安装目录")
	}
	defer func() {
		clean, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = guestCommand(clean, `sudo -S -p '' /bin/rm -rf `+remote+adminPassword).Run()
	}()
	fmt.Fprintln(os.Stdout, "正在将开发组件送入已验证的离线虚拟 Mac…")
	fmt.Fprintln(os.Stdout, "CAGE_GUEST_STAGE macos tools")
	// Only the fixed subset of media is transferred. The IPSW stays on the host.
	tar := exec.CommandContext(ctx, "/usr/bin/tar", "--no-mac-metadata", "-cf", "-", "-C", kit, ".", "-C", cache, "node.tgz", "editor.tgz", "playwright.tgz", "chrome.dmg", "claude.dmg", "cli", "macfuse.pkg", "-C", componentStage, "native-components.json")
	tar.Env = append(os.Environ(), "COPYFILE_DISABLE=1")
	tar.Stderr = os.Stderr
	pipe, err := tar.StdoutPipe()
	if err != nil {
		return err
	}
	receiver := guestCommand(ctx, `umask 077; cd `+remote+`; /usr/bin/tar -xf -; mv macfuse.pkg project/macfuse.pkg`)
	receiver.Stdin = pipe
	if err = tar.Start(); err != nil {
		return err
	}
	recvErr := receiver.Run()
	_ = pipe.Close()
	if err := errors.Join(recvErr, tar.Wait()); err != nil {
		return fmt.Errorf("客机组件传输失败：%w", err)
	}
	// The staging directory belongs exclusively to the provisioning account.
	// Before invoking the installer, root takes ownership and restricts access.
	auth := guestCommand(ctx, `umask 077; cat > `+remote+`/login-input; sudo -S -p '' /usr/sbin/chown -R root:wheel `+remote+adminPassword)
	auth.Stdin = io.MultiReader(bytes.NewReader(pw), strings.NewReader("\n"))
	if err := auth.Run(); err != nil {
		return errors.New("无法准备虚拟 Mac 账户")
	}
	cmd := guestCommand(ctx, `sudo -S -p '' /bin/sh -ec `+shellescape.Quote(`cd `+remote+`; exec 4<login-input; rm login-input; /usr/bin/perl ./native-component-manifest.pl source ./native-components.json .; /bin/bash ./provision-macos-workbench.sh `+remote+` `+i.ID.String()+` `+provision+` `+ordinaryUser+` <&4; /usr/bin/perl ./native-component-manifest.pl publish ./native-components.json .`)+adminPassword)
	cmd.Stdout = os.Stdout
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("安装虚拟 Mac 应用失败，可在工作台继续安装：%w", err)
	}
	ordinary, err := guestos.MacOSSSHArgs(profile, ordinaryUser)
	if err != nil {
		return err
	}
	verify := exec.CommandContext(ctx, "/usr/bin/ssh", append(ordinary, "-T", "cage-macos", "--",
		`test "$(id -un)" = `+ordinaryUser+` && test "$(uname -s)" = Darwin && ! id -Gn | /usr/bin/grep -qw admin`)...)
	if err := verify.Run(); err != nil {
		return errors.New("普通客机账户无法通过已验证的 SSH 通道连接，已保留安装进度")
	}
	key, err := guestos.MacOSInstallHostKey(profile)
	if err != nil {
		return err
	}
	// Remove the root-owned media before stopping. No user data is in this dir.
	if err := guestCommand(ctx, `sudo -S -p '' /bin/rm -rf `+remote+adminPassword).Run(); err != nil {
		return err
	}
	fmt.Fprintln(os.Stdout, "CAGE_GUEST_STAGE macos services")
	if err := l.Stop(ctx, "mac"); err != nil {
		return err
	}
	if err := guestos.BindNewMacOSProfile(profile, ordinaryUser, key, i.ID); err != nil {
		return err
	}
	fmt.Fprintln(os.Stdout, "macOS 客机应用已安装，自动登录已准备。Cage 将继续完成首次授权。")
	return nil
}
