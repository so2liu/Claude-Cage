#!/bin/bash
# Assemble the guest-only workbench controller alongside the pinned browser media.
set -euo pipefail
[[ $# == 1 ]]
src=$(cd "$(dirname "$0")" && pwd)
modules="$src/../../internal/cage"
dest="$1"
mkdir -p "$dest/modules" "$dest/split-extension"
install -m 644 "$src"/{macos-cli-browser,macos-native-browser,macos-browser-service,macos-phone-browser,windows-browser,windows-browser-extensions}.mjs "$dest/"
install -m 644 "$modules"/browser-{clipboard,uploads,downloads,cookies,extensions,vault,phone,phone-network}.mjs "$dest/modules/"
install -m 644 "$modules"/split-extension/{manifest.json,control.html,control.js,settings.html} "$dest/split-extension/"

install -m 644 "$modules/tailnet-client.mjs" "$dest/"
bash "$src/../build-tailnet.sh" darwin arm64 "$dest/cage-tailnet"
