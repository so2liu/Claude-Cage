#!/bin/bash
# Runs through the verified provisioning identity of an already bound Mac.
# The ordinary account and its home directory are never initialized here.
set -euo pipefail
export PATH=/usr/bin:/bin:/usr/sbin:/sbin
[[ $# == 3 && $1 == /* && $2 =~ ^[a-z][a-z0-9_]{0,30}$ && ( $3 == components || $3 == tools ) ]]
[[ $EUID == 0 && $(uname -sm) == 'Darwin arm64' && $(sysctl -n hw.model) =~ ^VirtualMac[0-9]+,[0-9]+$ ]]
[[ -f /var/db/cage-guest-os && ! -L /var/db/cage-guest-os && $(stat -f %u /var/db/cage-guest-os) == 0 && $(cat /var/db/cage-guest-os) == macos ]]
lock=/var/run/cage-component-update.lock
[[ ! -L $lock && ( ! -e $lock || ( -f $lock && $(stat -f %u "$lock") == 0 ) ) ]]
exec 9>"$lock"
/usr/bin/lockf -s -t 0 9 || { echo '另一项组件更新仍在运行，请稍后重试。' >&2; exit 1; }
umask 022
kit=$(cd -P "$1" && pwd)
# Helpers run as the ordinary guest user and need a traversable working directory.
cd /
user=$2
mode=$3
uid=$(id -u "$user")
[[ $uid -ge 500 && " $(id -Gn "$user") " != *' admin '* ]]
[[ $(stat -f %u "$kit") == 0 && -z $(find "$kit" \( -type l -o ! -user root -o -perm -0020 -o -perm -0002 \) -print -quit) ]]
manifest="$kit/native-components.json"
/usr/bin/perl "$kit/native-component-manifest.pl" source "$manifest" "$kit"
backup=$(mktemp -d /private/var/tmp/cage-components-backup.XXXXXXXX)
chmod 700 "$backup"
changed=()
existed=()
loaded=()
previous_loaded=()
apps=()
app_existed=()
app_applied=0
cli_previous=''
if [[ -L /usr/local/libexec/cage-cli ]]; then cli_previous=$(readlink /usr/local/libexec/cage-cli); fi

finish() {
    local status=$? index target label
    trap - EXIT
    if (( status != 0 )); then
        for ((index=0; index<${#loaded[@]}; index++)); do
            label=${loaded[$index]}
            sudo -Hu "$user" /bin/launchctl bootout "gui/$uid/$label" >/dev/null 2>&1 || true
        done
        for ((index=${#changed[@]}-1; index>=0; index--)); do
            target=${changed[$index]}
            if [[ ${existed[$index]} == yes ]]; then
                mv -f "$backup/$index" "$target" || status=1
            elif [[ -f $target && ! -L $target ]]; then
                rm "$target" || status=1
            fi
        done
        for ((index=app_applied-1; index>=0; index--)); do
            target=${apps[$index]}
            if [[ -d $target && ! -L $target ]]; then rm -rf "$target"; fi
            if [[ ${app_existed[$index]} == yes ]]; then mv "$backup/old-app-$index" "$target" || status=1; fi
        done
        if [[ -n $cli_previous ]]; then
            ln -s "$cli_previous" "$backup/cli-previous"
            mv -fh "$backup/cli-previous" /usr/local/libexec/cage-cli || status=1
        fi
        for ((index=0; index<${#previous_loaded[@]}; index++)); do
            label=${previous_loaded[$index]}
            if ! sudo -Hu "$user" /bin/launchctl bootstrap "gui/$uid" "/Users/$user/Library/LaunchAgents/$label.plist" >/dev/null 2>&1; then status=1; fi
        done
    fi
    if (( status == 0 )); then
        rm -rf "$backup"
    else
        printf '组件更新未完成。已恢复可恢复的原文件，备份位于 %s。可以在 Cage 重试。\n' "$backup" >&2
    fi
    exit "$status"
}
trap finish EXIT
trap 'exit 1' HUP INT TERM

# Verify app ownership before stopping the corresponding GUI helpers.
for app in "$kit"/*.app "$kit"/project/*.app; do
    [[ -d $app ]] || continue
    identifier=$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' "$app/Contents/Info.plist")
    [[ $identifier == dev.cage.* ]]
    codesign --verify --deep --strict -R "=identifier \"$identifier\"" "$app"
    target="/Applications/$(basename "$app")"
    if [[ -e $target ]]; then
        [[ -d $target && ! -L $target ]]
        # Missing or damaged installed files are repairable. The incoming app
        # is fully verified, and an existing signing team remains a constraint.
        previous_team=$(codesign -dv "$target" 2>&1 | sed -n 's/^TeamIdentifier=//p' || true)
        next_team=$(codesign -dv "$app" 2>&1 | sed -n 's/^TeamIdentifier=//p')
        [[ -z $previous_team || $previous_team == not\ set || $previous_team == "$next_team" ]]
    fi
    index=${#apps[@]}
    ditto "$app" "$backup/new-app-$index"
    chmod -R a+rX,go-w "$backup/new-app-$index"
    codesign --verify --deep --strict "$backup/new-app-$index"
    apps+=("$target")
    if [[ -d $target ]]; then app_existed+=(yes); else app_existed+=(no); fi
done
for label in dev.cage.GuestClaudeWindow dev.cage.GuestChromeWindow dev.cage.GuestClipboard dev.cage.GuestBrowser; do
    if sudo -Hu "$user" /bin/launchctl print "gui/$uid/$label" >/dev/null 2>&1; then
        loaded+=("$label")
        previous_loaded+=("$label")
        sudo -Hu "$user" /bin/launchctl bootout "gui/$uid/$label"
    fi
done
# Project shares and workbench terminals have already been closed by the host.
while read -r pid owner executable; do
    if [[ $owner == "$uid" && $executable == /Applications/CageProjectAccess.app/Contents/MacOS/CageProjectAccess ]]; then kill -TERM "$pid" 2>/dev/null || true; fi
done < <(ps -axo pid=,uid=,comm=)

for ((index=0; index<${#apps[@]}; index++)); do
    target=${apps[$index]}
    if [[ ${app_existed[$index]} == yes ]]; then mv "$target" "$backup/old-app-$index"; fi
    app_applied=$((index+1))
    mv "$backup/new-app-$index" "$target"
done

while IFS= read -r -d '' source && IFS= read -r -d '' target; do
    # The CLI installer publishes a new immutable helper directory and moves
    # its owned symlink. It also generates the verified Python entry shebang.
    [[ $target != /usr/local/libexec/cage-cli/* && $target != /Applications/* ]] || continue
    parent=$(dirname "$target")
    check=$parent
    while [[ $check != / ]]; do
        [[ ! -L $check ]]
        check=$(dirname "$check")
    done
    mkdir -p "$parent"
    [[ ! -L $target && ( ! -e $target || -f $target ) ]]
    if [[ -f $target ]] && cmp -s "$kit/$source" "$target"; then continue; fi
    index=${#changed[@]}
    if [[ -f $target ]]; then
        cp -p "$target" "$backup/$index"
        existed+=(yes)
    else
        existed+=(no)
    fi
    changed+=("$target")
    stage=$(mktemp "$parent/.cage-component.XXXXXXXX")
    cp -p "$kit/$source" "$stage"
    case "$target" in
        */controller/cage-tailnet|/usr/local/libexec/cage-register-*|/usr/local/libexec/cage-macos-editor|/usr/local/libexec/cage-sshfs|/usr/local/libexec/cage-macos-terminal|/usr/local/libexec/cage-finish-initial-setup|/usr/local/libexec/cage-macos-ipv6-check) chmod 755 "$stage" ;;
        *) chmod a+r,go-w "$stage" ;;
    esac
    mv -f "$stage" "$target"
done < <(/usr/bin/perl "$kit/native-component-manifest.pl" targets "$manifest" "$kit")

if [[ $mode == tools ]]; then
    bash "$kit/install-macos-node.sh" "$kit/node.tgz"
    bash "$kit/install-macos-browser.sh" "$kit" --update-tools
    bash "$kit/cli/install-macos-cli.sh" "$kit/cli"
else
    bash "$kit/install-macos-cli.sh" "$kit" --components
fi
for app in "$kit"/*.app "$kit"/project/*.app; do
    [[ -d $app ]] || continue
    codesign --verify --deep --strict "/Applications/$(basename "$app")"
done
# Registration can migrate an existing agent's lifecycle settings. Restore
# those files with the matching binaries if activation or publication fails.
for label in dev.cage.GuestClaudeWindow dev.cage.GuestChromeWindow dev.cage.GuestClipboard dev.cage.GuestBrowser; do
    target="/Users/$user/Library/LaunchAgents/$label.plist"
    [[ ! -L $target && ( ! -e $target || ( -f $target && $(stat -f %u "$target") == "$uid" ) ) ]]
    index=${#changed[@]}
    if [[ -f $target ]]; then
        cp -p "$target" "$backup/$index"
        existed+=(yes)
    else
        existed+=(no)
    fi
    changed+=("$target")
done
sudo -Hu "$user" /usr/local/libexec/cage-register-claude-window --prepare
sudo -Hu "$user" /usr/local/libexec/cage-register-chrome-window --prepare
sudo -Hu "$user" /usr/local/libexec/cage-register-clipboard --prepare
sudo -Hu "$user" /usr/local/libexec/cage-register-browser --prepare
# New helpers are activated for an already logged-in GUI user as well.
if [[ $(stat -f %u /dev/console) == "$uid" ]]; then
    for label in dev.cage.GuestClaudeWindow dev.cage.GuestChromeWindow dev.cage.GuestClipboard dev.cage.GuestBrowser; do
        if [[ " ${loaded[*]-} " != *" $label "* ]]; then loaded+=("$label"); fi
    done
fi
for ((index=0; index<${#loaded[@]}; index++)); do
    label=${loaded[$index]}
    sudo -Hu "$user" /bin/launchctl bootstrap "gui/$uid" "/Users/$user/Library/LaunchAgents/$label.plist"
done
/usr/bin/perl "$kit/native-component-manifest.pl" publish "$manifest" "$kit"
printf '%s\n' '原生客机组件已更新并通过文件核验。'
