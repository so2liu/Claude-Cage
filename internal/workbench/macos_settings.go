package workbench

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"al.essio.dev/pkg/shellescape"
	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/guestos"
)

// Apply the same configured values that the independent safety probe reads.
// Only the provisioning identity changes the system zone; the ordinary account
// owns its language preferences and never receives administrator credentials.
func (s *Server) applyMacOSGuestSettings(ctx context.Context, cfg cage.Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	user, err := guestos.MacOSProvisionUser(s.profile)
	if err != nil {
		return err
	}
	args, err := guestos.MacOSSSHArgs(s.profile, user)
	if err != nil {
		return err
	}
	zone := cfg.Timezone
	if macOSZone(zone) == "UTC" {
		zone = "GMT" // systemsetup rejects the otherwise valid IANA Etc/UTC alias.
	}
	op := shellescape.QuoteCommand([]string{"/usr/bin/sudo", "-S", "-p", "", "/usr/sbin/systemsetup", "-settimezone", zone})
	cmd := exec.CommandContext(ctx, sshExecutable(), append(args, "-T", s.sshTarget(), "--", "exec "+op+` < "$HOME/password"`)...)
	cmd.Env = sshEnvironment()
	if err = cmd.Run(); err != nil {
		return fmt.Errorf("无法应用 macOS 系统时区：%w", err)
	}
	locale := strings.TrimSuffix(cfg.Locale, ".UTF-8")
	language := strings.ReplaceAll(locale, "_", "-")
	script := "set -e\n" + shellescape.QuoteCommand([]string{"/usr/bin/defaults", "write", "NSGlobalDomain", "AppleLocale", "-string", locale}) + "\n" +
		shellescape.QuoteCommand([]string{"/usr/bin/defaults", "write", "NSGlobalDomain", "AppleLanguages", "-array", language}) + "\n"
	// SSH and project terminals start new zsh processes. CoreFoundation's
	// AppleLocale alone does not set libc/Node's process locale. Preserve the
	// user's existing shell configuration and add one idempotent managed hook.
	environment := "export LANG=" + shellescape.Quote(cfg.Locale) + " LC_ALL=" + shellescape.Quote(cfg.Locale) + " TZ=" + shellescape.Quote(cfg.Timezone) + "\n" +
		`export PATH="/usr/local/libexec/cage-cli/bin:/usr/local/bin:$PATH"` + "\n"
	script += `dir="$HOME/Library/Application Support/Cage"
test ! -L "$dir" && test ! -L "$HOME/.zshenv" && test ! -L "$HOME/.zprofile"
mkdir -p "$dir"
test ! -L "$dir/environment.sh"
umask 077
` + "printf '%s' " + shellescape.Quote(environment) + ` > "$dir/environment.sh"
hook='. "$HOME/Library/Application Support/Cage/environment.sh"'
for file in "$HOME/.zshenv" "$HOME/.zprofile"; do
    if ! test -f "$file" || ! /usr/bin/grep -Fqx "$hook" "$file"; then
        printf '\n%s\n' "$hook" >> "$file"
    fi
done
`
	if _, err = s.guestControl(ctx, script, "macos-system-settings"); err != nil {
		return fmt.Errorf("无法应用 macOS 系统语言：%w", err)
	}
	return nil
}

// During the closed startup gate macOS caches negative DNS answers. Its fixed
// safety probe does not use that resolver, so a successful grant must also
// discard those stale answers before ordinary applications resume.
func (s *Server) resetMacOSDNSCache(ctx context.Context) error {
	user, err := guestos.MacOSProvisionUser(s.profile)
	if err != nil {
		return err
	}
	args, err := guestos.MacOSSSHArgs(s.profile, user)
	if err != nil {
		return err
	}
	op := shellescape.QuoteCommand([]string{"/usr/bin/sudo", "-S", "-p", "", "/bin/sh", "-ec", "/usr/bin/dscacheutil -flushcache; /usr/bin/killall -HUP mDNSResponder"})
	cmd := exec.CommandContext(ctx, sshExecutable(), append(args, "-T", s.sshTarget(), "--", "exec "+op+` < "$HOME/password"`)...)
	cmd.Env = sshEnvironment()
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("无法刷新 macOS 客机 DNS 缓存：%w", err)
	}
	return nil
}
