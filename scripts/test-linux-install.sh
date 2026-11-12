#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
[[ -f /.dockerenv && $(id -u) == 0 ]] || { echo 'Run this destructive package test as root in a disposable Docker container.' >&2; exit 2; }
[[ $# == 1 && -f "$1" ]] || { echo 'Usage: test-linux-install.sh /path/to/Cage-linux-amd64.deb' >&2; exit 2; }
package=$(realpath "$1")
test_user=cage-package-test
id "$test_user" >/dev/null 2>&1 || useradd -m -s /bin/bash "$test_user"
test_home=$(getent passwd "$test_user" | cut -d: -f6)
report_dir="$test_home/reports"
profile="$test_home/upgrade-guard/config.json"
cleanup() {
  if [[ -x /usr/bin/claude-cage && ( -f "${profile%/*}/control.json" || -f "${profile%/*}/desktop-entry/control.json" ) ]]; then
    runuser -u "$test_user" -- env CLAUDE_CAGE_CONFIG="$profile" /usr/bin/claude-cage prepare-update --json >/dev/null || true
  fi
}
trap cleanup EXIT
install -d -m 700 -o "$test_user" -g "$test_user" "$report_dir" "$test_home/.claude-cage" "$test_home/.claude-cage-workspace"
manifest="$report_dir/personal-data.json"
runuser -u "$test_user" -- node "$PWD/scripts/personal-data-fixture.mjs" seed "$test_home" "$manifest"
verify_personal_data() {
  runuser -u "$test_user" -- node "$PWD/scripts/personal-data-fixture.mjs" verify "$test_home" "$manifest"
}
wait_for_package_exit() {
  # prepare-update confirms lock release; the controller and its log relay
  # can still be exiting. Use the installer's own process guard as the barrier.
  for ((attempt = 0; attempt < 200; attempt++)); do
    if sh /var/lib/dpkg/info/claude-cage.preinst install >/dev/null 2>&1; then return; fi
    sleep 0.1
  done
  sh /var/lib/dpkg/info/claude-cage.preinst install
}
export DEBIAN_FRONTEND=noninteractive
apt-get install --reinstall -y "$package"
verify_personal_data
runuser -u "$test_user" -- /usr/lib/claude-cage/cage-desktop --self-test
runuser -u "$test_user" -- xvfb-run -a dbus-run-session -- node "$PWD/desktop/linux/test-smoke.mjs" /usr/lib/claude-cage/cage-desktop "$report_dir/first-install.json"
wait_for_package_exit
# Keep a real control service alive and confirm that preinst refuses replacement.
runuser -u "$test_user" -- env CLAUDE_CAGE_CONFIG="$profile" /usr/bin/claude-cage desktop --json > /dev/null
if dpkg -i "$package" > "$report_dir/running-upgrade.log" 2>&1; then
  echo 'FAIL: installer replaced a running Cage backend.' >&2
  exit 1
fi
grep -q 'Cage is running' "$report_dir/running-upgrade.log"
verify_personal_data
if dpkg --remove claude-cage > "$report_dir/running-remove.log" 2>&1; then
  echo 'FAIL: uninstaller removed a running Cage backend.' >&2
  exit 1
fi
grep -q 'Cage is running' "$report_dir/running-remove.log"
verify_personal_data
runuser -u "$test_user" -- env CLAUDE_CAGE_CONFIG="$profile" /usr/bin/claude-cage prepare-update --json > "$report_dir/guard-stop.json"
wait_for_package_exit
dpkg -i "$package"
verify_personal_data
runuser -u "$test_user" -- xvfb-run -a dbus-run-session -- node "$PWD/desktop/linux/test-smoke.mjs" /usr/lib/claude-cage/cage-desktop "$report_dir/after-reinstall.json"
wait_for_package_exit
dpkg --remove claude-cage
[[ ! -e /usr/bin/cage-desktop && ! -e /usr/lib/claude-cage/cage ]] || { echo 'Uninstall left installed executables.' >&2; exit 1; }
verify_personal_data
printf '{"success":true,"install":true,"nativeWorkbench":true,"runningUpgradeRefused":true,"runningRemovalRefused":true,"reinstall":true,"uninstallPreservedData":true}\n' > "$report_dir/install.json"
printf 'PASS: package install, native workbench, running-update refusal, reinstall and data-preserving removal. Reports: %s\n' "$report_dir"
