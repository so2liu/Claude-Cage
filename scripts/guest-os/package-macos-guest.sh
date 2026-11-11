#!/bin/bash
# Bundle the first-party helpers. User-selected OS and application media are
# downloaded during installation and are never taken from a developer's VM.
set -euo pipefail
[[ $# == 1 && $1 == /* && ! -e $1 ]]
src=$(cd "$(dirname "$0")" && pwd)
dest=$1
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
mkdir -p "$dest/project"
bash "$src/build-macos-project-access.sh" "$dest/project/CageProjectAccess.app"
bash "$src/build-macos-claude-window.sh" "$dest/Cage Guest Window.app"
bash "$src/build-macos-claude-window.sh" "$dest/Cage Chrome Window.app" chrome
bash "$src/build-macos-clipboard.sh" "$dest/Cage Guest Clipboard.app"
xcrun clang -arch arm64 -O2 -fobjc-arc -framework Foundation "$src/macos-project-terminal.m" -o "$dest/project/cage-macos-terminal"
codesign -f -s "${CAGE_SIGNING_IDENTITY:--}" "$dest/project/cage-macos-terminal"
fuse="$work/macfuse.dmg"
if [[ -n ${CAGE_BUILD_MACFUSE_DMG:-} ]]; then
    fuse=$CAGE_BUILD_MACFUSE_DMG
else
    curl -fLsS --proto '=https' --proto-redir '=https' --retry 2 https://github.com/macfuse/macfuse/releases/download/macfuse-5.5.0/macfuse-5.5.0.dmg -o "$fuse"
fi
bash "$src/macos-fuse-media.sh" "$fuse" "$work/fuse.pkg"
pkgutil --expand-full "$work/fuse.pkg" "$work/sdk"
bash "$src/build-macos-sshfs.sh" "$work/sdk/Core.pkg/Payload/usr/local" "$dest/project/cage-sshfs" "$work/sshfs-source"
xcrun clang -arch arm64 -mmacosx-version-min=14.0 -O2 -fobjc-arc -framework Foundation -framework OpenDirectory \
    "$src/macos-setup-account.m" -o "$dest/cage-finish-initial-setup"
xcrun clang -arch arm64 -O2 "$src/macos-ipv6-check.c" -o "$dest/cage-macos-ipv6-check"
bash "$src/package-macos-browser.sh" "$dest/native-browser"
for f in prepare-macos-media.sh macos-cli.py prepare-macos-cli-kit.sh install-macos-cli.sh claude-desktop-macos.sh install-macos-browser.sh install-macos-node.sh install-macos-project-tools.sh register-macos-claude-window.sh register-macos-chrome-window.sh register-macos-clipboard.sh register-macos-browser.sh macos-browser.mjs cage-macos-editor macos-fuse-media.sh provision-macos-workbench.sh; do
    cp "$src/$f" "$dest/"
done
cp "$src/../../internal/cage/claude-account.py" "$src/../../internal/workbench/"{cli_channel.py,cli_channel.resume.md,claude_account_resume.md} "$dest/"
# Shared component maintenance is installed separately from first-run accounts.
cp "$src/maintain-macos-workbench.sh" "$src/native-component-manifest.pl" "$dest/"
cp "$src/configure-macos-session.sh" "$dest/"
# Final signing runs after every helper has been built. Component manifests are
# computed from these final bytes by RequiredNativeComponents at installation.
bash "$src/sign-macos-guest.sh" "$dest" "${CAGE_SIGNING_IDENTITY:--}"
# Bind the source distribution to the final Developer ID bytes, after the kit
# has signed SSHFS. The outer host App signature does not change this helper.
version=$(tr -d '\n\r' < "$src/../../VERSION")
node "$src/collect-macos-sshfs-sources.mjs" finish "$work/sshfs-source" "$dest/project/cage-sshfs" \
  "$dest/sources/cage-macos-guest-$version-source.tar.gz" "$dest/licenses/sshfs"
