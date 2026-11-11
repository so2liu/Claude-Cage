//go:build darwin && arm64 && cgo && !no_vz

package maclogin

import (
	"context"
	"os/exec"

	"al.essio.dev/pkg/shellescape"
	"github.com/sylvinus/agent-vm/internal/guestos"
)

// Read authorization state from the installed, root-owned executable. The
// ordinary GUI user's writable socket and shell startup files are not trusted
// to identify a destination for an administrator password.
func guestGUIProbeCommand(ctx context.Context, profile, administrator, user string, arguments ...string) (*exec.Cmd, error) {
	args, err := guestos.MacOSSSHArgs(profile, administrator)
	if err != nil {
		return nil, err
	}
	rootScript := `set -eu
app='/Applications/Cage Guest Window.app'
identifier=dev.cage.GuestClaudeWindow
if [ -d '/Applications/Cage Chrome Window.app' ]; then
    app='/Applications/Cage Chrome Window.app'
    identifier=dev.cage.GuestChromeWindow
fi
for file in "$app" "$app/Contents" "$app/Contents/MacOS" "$app/Contents/MacOS/CageGuestWindow"; do
    test ! -L "$file" && test "$(/usr/bin/stat -f %u "$file")" = 0 || exit 72
    mode=$(/usr/bin/stat -f %Lp "$file")
    test "$((0$mode & 022))" = 0 || exit 72
done
/usr/bin/codesign --verify --strict -R "=identifier \"$identifier\"" "$app" || exit 72
uid=$(/usr/bin/id -u ` + shellescape.Quote(user) + `)
test "$(/usr/bin/stat -f %u /dev/console)" = "$uid" || exit 73
exec /bin/launchctl asuser "$uid" /usr/bin/sudo -Hu ` + shellescape.Quote(user) + ` /usr/bin/env -i HOME=` + shellescape.Quote("/Users/"+user) + ` PATH=/usr/bin:/bin ` +
		`"$app/Contents/MacOS/CageGuestWindow" ` + shellescape.QuoteCommand(arguments)
	// An askpass helper keeps SSH stdin available to the probe. Its private
	// script contains only the existing credential's path, and is removed when
	// this invocation ends. Credentials remain inside the guest.
	loginScript := `set -eu
umask 077
probe_dir=$(/usr/bin/mktemp -d ` + shellescape.Quote("/Users/"+administrator+"/.cage-probe.XXXXXXXX") + `)
trap '/bin/rm -f "$probe_dir/askpass"; /bin/rmdir "$probe_dir"' EXIT
trap 'exit 1' HUP INT TERM
/usr/bin/printf '%s\n' '#!/bin/sh' ` + shellescape.Quote("exec /bin/cat "+shellescape.Quote("/Users/"+administrator+"/password")) + ` > "$probe_dir/askpass"
/bin/chmod 700 "$probe_dir/askpass"
SUDO_ASKPASS="$probe_dir/askpass" /usr/bin/sudo -A -p '' /bin/sh -c ` + shellescape.Quote(rootScript)
	command := "exec /bin/sh -c " + shellescape.Quote(loginScript)
	cmd := exec.CommandContext(ctx, "/usr/bin/ssh", append(args, "-T", "cage-macos", "--", command)...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LC_ALL=C"}
	return cmd, nil
}
