#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
if [[ "$(uname -s)" != Darwin ]]; then echo 'Cage.app must be built on macOS.' >&2; exit 1; fi
case "${1:-}" in ''|--install|--macos-trial|--install-macos-trial) ;; *) echo 'Usage: scripts/build-app.sh [--install|--macos-trial|--install-macos-trial]' >&2; exit 2 ;; esac
bundle_id=io.github.lee-xeo.claude-cage
app_name=Cage
if [[ "${1:-}" == *macos-trial ]]; then
  bundle_id+=.macos-trial
  app_name='Cage macOS'
fi
installed_app="$HOME/Applications/$app_name.app"
app_destination="${CAGE_BUILD_OUTPUT:-$PWD/_output/$app_name.app}"
if [[ "$app_destination" != /*.app ]]; then echo 'CAGE_BUILD_OUTPUT must be an absolute .app path.' >&2; exit 2; fi
check_destination() {
  local target="$1"
  [[ ! -e "$target" ]] && return 0
  if [[ "$(/usr/libexec/PlistBuddy -c 'Print :CFBundleIdentifier' "$target/Contents/Info.plist" 2>/dev/null)" != "$bundle_id" ]]; then
    echo 'Build destination is not a Cage application.' >&2; exit 1
  fi
  if ! codesign --verify --deep --strict "$target"; then
    echo '现有 Cage 应用包含修改内容或无法验证完整性，已保留。请将个人文件移出应用包，或选择新的构建目录后重试。' >&2
    exit 1
  fi
  local running_pids
  running_pids="$(/usr/sbin/lsof -t "$target/Contents/MacOS/Cage" "$target/Contents/Resources/cage" 2>/dev/null || true)"
  if [[ -n "$running_pids" ]]; then
    echo 'Cage is running from this destination. Quit it before replacing the App.' >&2; exit 1
  fi
}
check_destination "$app_destination"
if [[ "${1:-}" == --install* ]]; then check_destination "$installed_app"; fi
version="$(tr -d '\n\r' < VERSION)"
if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[A-Za-z0-9.-]+)?$ ]]; then echo 'Invalid VERSION.' >&2; exit 1; fi
short_version="${version%%-*}"
build_version="${CAGE_BUILD_NUMBER:-1}"
if [[ ! "$build_version" =~ ^[0-9]+(\.[0-9]+){0,2}$ ]]; then echo 'Invalid CAGE_BUILD_NUMBER.' >&2; exit 1; fi
identity="${CAGE_SIGNING_IDENTITY:--}"
export CAGE_SIGNING_IDENTITY="$identity"
# The host SDK may default to the developer's newer macOS version. Set the
# deployment target for the Go/cgo helper as well as the Swift launcher.
export MACOSX_DEPLOYMENT_TARGET=14.0
export CGO_CFLAGS="${CGO_CFLAGS:-} -mmacosx-version-min=14.0"
export CGO_LDFLAGS="${CGO_LDFLAGS:-} -mmacosx-version-min=14.0"
bash scripts/fetch-proxy-core.sh
if [[ "$(uname -m)" == arm64 ]]; then
  bash scripts/guest-os/build-macos-guest-core.sh "$version"
else
  make cage VERSION="$version"
fi
_output/bin/cage verify-web-assets
build_stage="$(mktemp -d "$PWD/_output/app-build.XXXXXX")"
install_stage=''
trap 'rm -rf "$build_stage"; if [[ -n "$install_stage" ]]; then rm -rf "$install_stage"; fi' EXIT
bundle="$build_stage/Cage.app"
mkdir -p "$bundle/Contents/MacOS" "$bundle/Contents/Resources"
swiftc -O -target "$(uname -m)-apple-macosx14.0" -parse-as-library -framework Cocoa -framework WebKit -framework Security -framework LocalAuthentication desktop/Cage.swift desktop/MacKeyboard.swift desktop/NativeInput.swift desktop/Updates.swift desktop/AccountDevice.swift desktop/MacGuestLogin.swift -o "$bundle/Contents/MacOS/Cage"
swiftc -O -target "$(uname -m)-apple-macosx14.0" -parse-as-library -framework Cocoa -framework Security desktop/Updates.swift desktop/UpdateInstaller.swift -o "$bundle/Contents/Resources/CageUpdateInstaller"
cp _output/bin/cage "$bundle/Contents/Resources/cage"
if [[ "$(uname -m)" == arm64 ]]; then "$bundle/Contents/Resources/cage" verify-macos-guest; fi
if [[ "$(uname -m)" == arm64 ]]; then
  bash scripts/guest-os/package-macos-guest.sh "$bundle/Contents/Resources/guest-os"
  mkdir -p "$bundle/Contents/Resources/licenses/sshfs"
  cp -R "$bundle/Contents/Resources/guest-os/licenses/sshfs/." "$bundle/Contents/Resources/licenses/sshfs/"
  # The kit signs all nested native code before this outer App is sealed.
  # Do not generate or reuse an unsigned component manifest here: installation
  # hashes the final signed kit, including each nested bundle's CodeResources.
fi
node scripts/bundle-cloud-kit.mjs "$bundle/Contents/Resources"
cp _output/proxy-core/mihomo "$bundle/Contents/Resources/mihomo"
for executable in "$bundle/Contents/MacOS/Cage" "$bundle/Contents/Resources/CageUpdateInstaller" "$bundle/Contents/Resources/cage" "$bundle/Contents/Resources/mihomo"; do
  minimum="$(otool -l "$executable" | awk '$1 == "minos" {print $2}')"
  if [[ -z "$minimum" ]] || ! awk -v version="$minimum" 'BEGIN {split(version,v,"."); exit !(v[1] < 14 || (v[1] == 14 && v[2] == 0 && v[3]+0 == 0))}'; then
    printf 'Unsupported deployment target %s in %s (expected macOS 14.0 or earlier).\n' "$minimum" "$executable" >&2
    exit 1
  fi
done
mkdir -p "$bundle/Contents/Resources/licenses/mihomo"
cp _output/proxy-core/LICENSE _output/proxy-core/NOTICE "$bundle/Contents/Resources/licenses/mihomo/"
node scripts/collect-app-licenses.mjs "$bundle/Contents/Resources/licenses"
swift desktop/Icon.swift logo.png _output/Cage.iconset
iconutil -c icns _output/Cage.iconset -o "$bundle/Contents/Resources/Cage.icns"
cat > "$bundle/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>CFBundleName</key><string>$app_name</string>
<key>CFBundleDisplayName</key><string>$app_name</string>
<key>CFBundleIdentifier</key><string>$bundle_id</string>
<key>CFBundleVersion</key><string>$build_version</string>
<key>CFBundleShortVersionString</key><string>$short_version</string>
<key>CageVersion</key><string>$version</string>
<key>CFBundleExecutable</key><string>Cage</string>
<key>CFBundleIconFile</key><string>Cage</string>
<key>CFBundlePackageType</key><string>APPL</string>
<key>LSMinimumSystemVersion</key><string>14.0</string>
<key>NSHighResolutionCapable</key><true/>
<key>NSAppTransportSecurity</key><dict><key>NSAllowsLocalNetworking</key><true/></dict>
</dict></plist>
PLIST
sign=(codesign --force --sign "$identity")
if [[ "$identity" != - ]]; then sign+=(--timestamp --options runtime); fi
"${sign[@]}" --entitlements third_party/lima/vz.entitlements "$bundle/Contents/Resources/cage"
if [[ "$(uname -m)" == arm64 ]]; then
  vm_bundle="$bundle/Contents/Resources/Cage Virtual Machine.app"
  mkdir -p "$vm_bundle/Contents/MacOS"
  cp "$bundle/Contents/Resources/cage" "$vm_bundle/Contents/MacOS/cage"
  cat > "$vm_bundle/Contents/Info.plist" <<'VMPLIST'
<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict>
<key>CFBundleIdentifier</key><string>io.github.lee-xeo.claude-cage.virtual-machine</string>
<key>CFBundleName</key><string>Cage Virtual Machine</string>
<key>CFBundleExecutable</key><string>cage</string>
<key>CFBundlePackageType</key><string>APPL</string>
<key>LSMinimumSystemVersion</key><string>14.0</string>
<key>LSUIElement</key><true/>
<key>NSHighResolutionCapable</key><true/>
</dict></plist>
VMPLIST
  "${sign[@]}" --entitlements third_party/lima/vz.entitlements "$vm_bundle"
fi
account_signing=("${sign[@]}")
if [[ -n "${CAGE_PROVISIONING_PROFILE:-}" ]]; then
  [[ "$identity" != - ]] || { echo 'Biometric provisioning requires a Developer ID signature.' >&2; exit 1; }
  signing_team="$(codesign -dv --verbose=4 "$bundle/Contents/Resources/cage" 2>&1 | sed -n 's/^TeamIdentifier=//p')"
  bash scripts/prepare-account-signing.sh "$CAGE_PROVISIONING_PROFILE" "$signing_team" "$build_stage/account.entitlements"
  cp "$CAGE_PROVISIONING_PROFILE" "$bundle/Contents/embedded.provisionprofile"
  account_signing+=(--entitlements "$build_stage/account.entitlements")
fi
"${sign[@]}" "$bundle/Contents/Resources/mihomo"
"${sign[@]}" "$bundle/Contents/Resources/CageUpdateInstaller"
"${account_signing[@]}" "$bundle/Contents/MacOS/Cage"
"${account_signing[@]}" "$bundle"
codesign --verify --deep --strict --verbose=2 "$bundle"
# Keep the working App until the replacement has passed signature verification.
check_destination "$app_destination"
mkdir -p "$(dirname "$app_destination")"
if [[ -e "$app_destination" ]]; then mv "$app_destination" "$build_stage/previous.app"; fi
if ! mv "$bundle" "$app_destination"; then
  if [[ -d "$build_stage/previous.app" ]]; then mv "$build_stage/previous.app" "$app_destination"; fi
  exit 1
fi
bundle="$app_destination"
printf 'Built: %s\n' "$bundle"
if [[ "${1:-}" == --install* ]]; then
  mkdir -p "$HOME/Applications"
  install_stage="$(mktemp -d "$HOME/Applications/.Cage-install.XXXXXX")"
  ditto "$bundle" "$install_stage/Cage.app"
  codesign --verify --deep --strict --verbose=2 "$install_stage/Cage.app"
  check_destination "$installed_app"
  if [[ -e "$installed_app" ]]; then mv "$installed_app" "$install_stage/previous.app"; fi
  if ! mv "$install_stage/Cage.app" "$installed_app"; then
    if [[ -d "$install_stage/previous.app" ]]; then mv "$install_stage/previous.app" "$installed_app"; fi
    exit 1
  fi
  printf 'Installed: %s\n' "$installed_app"
  # An installed build has one launchable copy. Remove the staging build too,
  # along with historical validation copies, after the installed App verifies.
  codesign --verify --deep --strict --verbose=2 "$installed_app"
  if [[ "${1:-}" == --install-macos-trial ]]; then
    # Normal release cleanup scans other Cage installations. The trial has a separate identity.
    if [[ ! "$bundle" -ef "$installed_app" ]]; then mv "$bundle" "$build_stage/installed-trial.app"; fi
    exit 0
  fi
  node scripts/cleanup-apps.mjs --keep "$installed_app" \
    --root "$bundle" --root "$PWD/_output" --root "$HOME/Applications" \
    --root /Applications --root "$HOME/.claude-cage-validation"
else
  if [[ -z "${CAGE_BUILD_OUTPUT:-}" && "${1:-}" != --macos-trial ]]; then
    node scripts/cleanup-apps.mjs --keep "$bundle" --root "$PWD/_output"
  fi
fi
