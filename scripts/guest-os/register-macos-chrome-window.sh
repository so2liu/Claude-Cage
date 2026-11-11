#!/bin/bash
# Run once as the intended guest GUI user after installing the signed helper.
set -euo pipefail
export PATH=/usr/bin:/bin:/usr/sbin:/sbin
[[ $(sysctl -n hw.model) =~ ^VirtualMac[0-9]+,[0-9]+$ && $(cat /var/db/cage-guest-os) == macos ]] || exit 2
[[ $# == 0 || ( $# == 1 && $1 == --prepare ) ]] || exit 2
uid=$(id -u)
[[ $uid -ge 500 && ( ${1:-} == --prepare || $(stat -f %u /dev/console) == "$uid" ) ]] || { echo 'Log into the guest as the intended desktop user.' >&2; exit 4; }
app='/Applications/Cage Chrome Window.app'
codesign --verify --strict -R '=identifier "dev.cage.GuestChromeWindow"' "$app"
label=dev.cage.GuestChromeWindow
cache="$HOME/Library/Caches/$label"
agent="$HOME/Library/LaunchAgents/$label.plist"
executable="$app/Contents/MacOS/CageGuestWindow"
umask 077
mkdir -p "$HOME/Library/LaunchAgents" "$cache"
[[ ! -L $cache && $(stat -f %u "$cache") == "$uid" ]] || exit 2
chmod 700 "$cache"
temp=$(mktemp "$cache/plist.XXXXXXXX")
legacy=$(mktemp "$cache/plist-legacy.XXXXXXXX")
trap 'rm -f "$temp" "$legacy"' EXIT
plutil -create xml1 "$temp"
plutil -insert Label -string "$label" "$temp"
plutil -insert ProgramArguments -xml '<array/>' "$temp"
plutil -insert ProgramArguments.0 -string "$executable" "$temp"
plutil -insert ProgramArguments.1 -string --chrome "$temp"
plutil -insert LimitLoadToSessionType -string Aqua "$temp"
plutil -insert Sockets -xml '<dict><key>Window</key><dict/></dict>' "$temp"
plutil -insert Sockets.Window.SockPathName -string "$cache/window.sock" "$temp"
plutil -insert Sockets.Window.SockPathMode -integer 384 "$temp"
cp "$temp" "$legacy"
# LaunchServices delivers file-open events to the same GUI process as the
# socket service. Keep that process alive before the first remote request.
plutil -insert RunAtLoad -bool true "$temp"
plutil -insert KeepAlive -bool true "$temp"
changed=0
if [[ -e $agent || -L $agent ]]; then
    [[ -f $agent && ! -L $agent && $(stat -f %u "$agent") == "$uid" ]] || exit 2
    if ! cmp -s "$agent" "$temp"; then
        if ! cmp -s "$agent" "$legacy"; then
            echo 'An existing LaunchAgent has different settings.' >&2
            exit 2
        fi
        changed=1
    fi
fi
if [[ ${1:-} != --prepare && $changed == 1 ]] && launchctl print "gui/$uid/$label" >/dev/null 2>&1; then
    launchctl bootout "gui/$uid/$label"
fi
mv -f "$temp" "$agent"
[[ ${1:-} == --prepare ]] && exit 0
if ! launchctl print "gui/$uid/$label" >/dev/null 2>&1; then
    launchctl bootstrap "gui/$uid" "$agent"
fi
