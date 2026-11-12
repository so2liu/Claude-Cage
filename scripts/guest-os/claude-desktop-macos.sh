#!/bin/bash
# Run inside a Cage macOS guest. Install accepts a local official DMG.
set -euo pipefail
export PATH=/usr/bin:/bin:/usr/sbin:/sbin

fail() { printf '%s\n' "$*" >&2; exit 1; }

case "${1:-}" in
  inspect|launch) [[ $# == 1 ]] || fail "Usage: $0 $1" ;;
  install) [[ $# == 2 ]] || fail "Usage: $0 install /path/to/Claude.dmg" ;;
  verify-media) [[ $# == 3 && $3 =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || fail "Usage: $0 verify-media /path/to/Claude.dmg version" ;;
  *) fail "Usage: $0 inspect | install /path/to/Claude.dmg | launch" ;;
esac

app=/Applications/Claude.app
bundle_id=com.anthropic.claudefordesktop
team=Q6L2SF6YDW
requirement="anchor apple generic and identifier \"$bundle_id\" and certificate leaf[subject.OU] = \"$team\" and certificate 1[field.1.2.840.113635.100.6.2.6] exists and certificate leaf[field.1.2.840.113635.100.6.1.13] exists"

info() {
  plutil -extract "$2" raw -o - "$1/Contents/Info.plist"
}

verify_app() {
  [[ -d $1 && ! -L $1 ]] || fail "Expected a Claude.app directory at $1."
  [[ $(info "$1" CFBundleIdentifier) == "$bundle_id" ]] || fail "Unexpected Claude bundle identifier."
  codesign --verify --deep --strict "$1"
  codesign --verify --strict -R "=$requirement" "$1"
}

if [[ $1 == verify-media ]]; then
  [[ $2 == /* && -f $2 && ! -L $2 ]] || fail 'Supply a downloaded Claude DMG.'
  stage=$(mktemp -d "${TMPDIR:-/tmp}/claude-media.XXXXXXXX")
  mountdir="$stage/media"
  mkdir "$mountdir"
  mounted=false
  cleanup() {
    if [[ $mounted == true ]]; then
      hdiutil detach "$mountdir" >/dev/null || return
    fi
    rm -rf "$stage"
  }
  trap cleanup EXIT
  trap 'exit 1' HUP INT TERM
  hdiutil attach -readonly -nobrowse -noautoopen -mountpoint "$mountdir" "$2" >/dev/null
  mounted=true
  verify_app "$mountdir/Claude.app"
  [[ $(info "$mountdir/Claude.app" CFBundleShortVersionString) == "$3" ]] || fail 'Claude media version differs from the official release.'
  exit 0
fi

[[ $(uname -s) == Darwin ]] || fail "This helper requires a Cage macOS guest."
model=$(sysctl -n hw.model)
[[ $model =~ ^VirtualMac[0-9]+,[0-9]+$ ]] || fail "This Mac is not a virtual Mac. Run this helper inside the Cage guest."
marker=/var/db/cage-guest-os
[[ -f $marker && ! -L $marker && $(stat -f '%u' "$marker") == 0 ]] ||
  fail "Missing root-owned Cage guest marker."
mode=$(stat -f '%OLp' "$marker")
(( (8#$mode & 022) == 0 )) && [[ $(cat "$marker") == macos ]] ||
  fail "Invalid Cage guest marker."

version_ge() {
  local i left right
  local -a a b
  [[ $1 =~ ^[0-9]+\.[0-9]+\.[0-9]+$ && $2 =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || return 1
  IFS=. read -r -a a <<< "$1"
  IFS=. read -r -a b <<< "$2"
  for ((i=0; i<3; i++)); do
    left=$((10#${a[i]})); right=$((10#${b[i]}))
    (( left == right )) || { (( left > right )); return; }
  done
}

report() {
  printf 'status=%s\nversion=%s\nbuild=%s\nbundle_id=%s\nteam=%s\n' \
    "$1" "$(info "$app" CFBundleShortVersionString)" "$(info "$app" CFBundleVersion)" "$bundle_id" "$team"
}

case "$1" in
  inspect)
    [[ -e $app || -L $app ]] || { printf 'status=absent\n'; exit 3; }
    verify_app "$app"
    report installed
    ;;
  install)
    [[ $EUID == 0 ]] || fail "Run install as root inside the guest."
    dmg=$2
    [[ $dmg == /* && -f $dmg ]] || fail "Supply the absolute path of the downloaded Claude DMG."
    exec 9>/var/run/cage-claude-install.lock
    lockf -s -t 0 9 || fail "A Claude installation is in progress."
    stage=$(mktemp -d /Applications/.cage-claude.XXXXXXXX)
    mountdir="$stage/media"
    mkdir "$mountdir"
    cleanup() {
      # Mounts are read-only. Keep the directory if the system cannot detach.
      if [[ $(stat -f '%d' "$mountdir") != "$(stat -f '%d' "$stage")" ]]; then
        hdiutil detach "$mountdir" >/dev/null || { printf 'Unable to detach %s\n' "$mountdir" >&2; return; }
      fi
      rm -rf "$stage"
    }
    trap cleanup EXIT
    trap 'exit 1' HUP INT TERM
    hdiutil attach -readonly -nobrowse -noautoopen -mountpoint "$mountdir" "$dmg" >/dev/null
    source_app="$mountdir/Claude.app"
    verify_app "$source_app"
    # renamex_np publishes the verified copy on the same filesystem. With an
    # existing app, RENAME_SWAP retains it in staging until the swap succeeds.
    rename_flag=4 # RENAME_EXCL
    if [[ -e $app || -L $app ]]; then
      verify_app "$app"
      if version_ge "$(info "$app" CFBundleShortVersionString)" "$(info "$source_app" CFBundleShortVersionString)"; then
        report installed
        exit 0
      fi
      if pgrep -f '^/Applications/Claude\.app/Contents/MacOS/Claude([[:space:]]|$)' >/dev/null; then
        fail "Quit the running guest Claude app before installing another version."
      fi
      rename_flag=2 # RENAME_SWAP
    fi
    staged_app="$stage/Claude.app"
    ditto "$source_app" "$staged_app"
    chown -R -P root:wheel "$staged_app"
    verify_app "$staged_app"
    /usr/bin/osascript -l JavaScript - "$staged_app" "$app" "$rename_flag" <<'JXA'
ObjC.bindFunction("renamex_np", ["int", ["char *", "char *", "uint32_t"]]);
function run(args) {
  if ($.renamex_np(args[0], args[1], Number(args[2])) !== 0) {
    throw Error("Could not publish Claude.app; previous installation preserved.");
  }
}
JXA
    report installed
    ;;
  launch)
    verify_app "$app"
    gui_uid=$(stat -f '%u' /dev/console)
    if (( gui_uid < 500 )) || ! launchctl print "gui/$gui_uid" >/dev/null 2>&1; then
      printf 'status=requires_gui_login\nLog in to the guest macOS desktop first.\n' >&2
      exit 4
    fi
    if [[ $EUID == 0 ]]; then
      launchctl asuser "$gui_uid" sudo -n -H -u "#$gui_uid" -- /usr/bin/open "$app"
    elif [[ $EUID == "$gui_uid" ]]; then
      /usr/bin/open "$app"
    else
      fail "Run launch as root or the guest console user."
    fi
    for ((attempt=0; attempt<20; attempt++)); do
      if pid=$(pgrep -o -U "$gui_uid" -f '^/Applications/Claude\.app/Contents/MacOS/Claude([[:space:]]|$)'); then
        report running
        printf 'pid=%s\n' "$pid"
        exit 0
      fi
      sleep 1
    done
    fail "The Claude application process did not start in the guest GUI session."
    ;;
esac
