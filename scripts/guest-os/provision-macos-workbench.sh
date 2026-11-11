#!/bin/bash
# Executed as root over the authenticated provisioning channel of a fresh VM.
set -euo pipefail
export PATH=/usr/bin:/bin:/usr/sbin:/sbin
[[ $EUID == 0 && $(uname -sm) == 'Darwin arm64' && $(sysctl -n hw.model) =~ ^VirtualMac[0-9]+,[0-9]+$ ]]
kit=$(cd "$1" && pwd -P)
[[ $(stat -f %u "$kit") == 0 ]]
umask 022
marker=/var/db/cage-guest-os
if [[ ! -e $marker ]]; then echo macos > "$marker"; fi
[[ -f $marker && ! -L $marker && $(stat -f %u "$marker") == 0 && $(cat "$marker") == macos ]]
# The password is supplied on stdin and is never a command line argument.
IFS= read -r login_password
[[ -n $login_password ]]
cd /
provision=$3
[[ $provision == cageprovision ]]
ordinary=${4:-cage}
[[ $ordinary =~ ^[a-z][a-z0-9_]{0,30}$ && $ordinary != root && $ordinary != cageprovision && $ordinary != cagesetup ]]
full_name=$ordinary
/usr/bin/dscl . -create "/Users/$provision" IsHidden 1
claim=/var/db/cage-install-$ordinary
if [[ ! -e $claim ]]; then
    if id $ordinary >/dev/null 2>&1 || [[ -e /Users/$ordinary ]]; then echo 'An existing account uses the Cage guest account name.' >&2; exit 1; fi
    install -m 600 /dev/null "$claim"
fi
[[ -f $claim && ! -L $claim && $(stat -f %u "$claim") == 0 ]]
if ! id $ordinary >/dev/null 2>&1; then
    [[ ! -e /Users/$ordinary ]]
    printf '%s\n' "$login_password" | /usr/sbin/sysadminctl -addUser $ordinary -fullName "$full_name" -password - -home /Users/$ordinary
    id $ordinary >/dev/null
    if [[ ! -e /Users/$ordinary ]]; then /usr/sbin/createhomedir -c -u $ordinary; fi
fi
[[ $(dscl . -read /Users/$ordinary NFSHomeDirectory) == "NFSHomeDirectory: /Users/$ordinary" && " $(id -Gn $ordinary) " != *' admin '* ]]
if ! printf '%s\n' "$login_password" | dscl /Local/Default -authonly $ordinary >/dev/null; then
    echo '保存的客机凭据与安装账户不匹配，请核对加密保险箱。' >&2
    exit 1
fi
install -d -m 755 /usr/local/libexec
install -m 755 "$kit/cage-finish-initial-setup" /usr/local/libexec/cage-finish-initial-setup
printf '%s\n' "$login_password" | /usr/local/libexec/cage-finish-initial-setup prepare "$2" "$ordinary"
install -m 755 "$kit/configure-macos-session.sh" /usr/local/libexec/cage-configure-session
printf '%s\n' "$login_password" | /usr/local/libexec/cage-configure-session "$ordinary" "$2"
unset login_password
[[ ! -L /Users/$ordinary && $(stat -f %u /Users/$ordinary) == "$(id -u $ordinary)" ]]
# Account-controlled paths are opened with that account's privileges.
cat "/Users/$provision/.ssh/authorized_keys" | sudo -Hu $ordinary /bin/sh -ec '
    umask 077
    mkdir -p "$HOME/.ssh"
    file="$HOME/.ssh/authorized_keys"
    if [ -e "$file" ] || [ -L "$file" ]; then cmp -s - "$file"; else (set -C; cat > "$file"); fi'

# Clean Apple restores restrict remote login to the guest's SSH access group.
if dscl . -read /Groups/com.apple.access_ssh >/dev/null 2>&1; then
    /usr/sbin/dseditgroup -o edit -a $ordinary -t user com.apple.access_ssh
fi
cd "$kit"
bash install-macos-project-tools.sh "$kit/project"
bash install-macos-node.sh "$kit/node.tgz"
bash install-macos-browser.sh "$kit"
bash cli/install-macos-cli.sh "$kit/cli"
bash claude-desktop-macos.sh install "$kit/claude.dmg"
install -m 755 cage-macos-ipv6-check /usr/local/libexec/cage-macos-ipv6-check
app='/Applications/Cage Guest Window.app'
if [[ ! -e $app ]]; then ditto 'Cage Guest Window.app' "$app"; fi
codesign --verify --strict -R '=identifier "dev.cage.GuestClaudeWindow"' "$app"
chmod -R a+rX,go-w "$app"
install -m 755 register-macos-claude-window.sh /usr/local/libexec/cage-register-claude-window
install -m 755 register-macos-chrome-window.sh /usr/local/libexec/cage-register-chrome-window
chrome_app='/Applications/Cage Chrome Window.app'
if [[ ! -e $chrome_app ]]; then ditto 'Cage Chrome Window.app' "$chrome_app"; fi
codesign --verify --strict -R '=identifier "dev.cage.GuestChromeWindow"' "$chrome_app"
chmod -R a+rX,go-w "$chrome_app"
clipboard_app='/Applications/Cage Guest Clipboard.app'
if [[ ! -e $clipboard_app ]]; then ditto 'Cage Guest Clipboard.app' "$clipboard_app"; fi
codesign --verify --deep --strict -R '=identifier "dev.cage.GuestClipboard"' "$clipboard_app"
chmod -R a+rX,go-w "$clipboard_app"
install -m 755 register-macos-clipboard.sh /usr/local/libexec/cage-register-clipboard
install -m 755 register-macos-browser.sh /usr/local/libexec/cage-register-browser
# Install the user's actual socket-activated LaunchAgent before the first Aqua
# login. launchd reads it during login, including while Setup Assistant runs.
cd /
sudo -Hu $ordinary /usr/local/libexec/cage-register-claude-window --prepare
sudo -Hu $ordinary /usr/local/libexec/cage-register-chrome-window --prepare
sudo -Hu $ordinary /usr/local/libexec/cage-register-clipboard --prepare
sudo -Hu $ordinary /usr/local/libexec/cage-register-browser --prepare
