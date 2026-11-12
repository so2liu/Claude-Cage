#!/bin/bash
# Download application media on the host. The guest's external network stays closed.
set -euo pipefail
export PATH=/usr/bin:/bin:/usr/sbin:/sbin
[[ $# == 1 && $1 == /* ]]
media=$1
src=$(cd "$(dirname "$0")" && pwd)
mkdir -p "$media"
export CAGE_HOST_DOWNLOAD_CACHE="$media/.downloads"
fetch() {
    if [[ -n ${CAGE_HOST_DOWNLOADER:-} ]]; then
        if [[ -n ${5:-} ]]; then
            "$CAGE_HOST_DOWNLOADER" host-download "$1" "$2" "$3" "${4:-}" "$CAGE_HOST_DOWNLOAD_CACHE" "$5"
        else
            "$CAGE_HOST_DOWNLOADER" host-download "$1" "$2" "$3" "${4:-}"
        fi
    else
        # Standalone source builds can supply curl proxy environment variables.
        curl --disable -fsSL --proto '=https' --proto-redir '=https' --retry 2 --connect-timeout 15 --speed-time 60 --speed-limit 1 -H "Accept: ${4:-*/*}" "$1" -o "$2"
    fi
}
desktop_media() (
    local temporary version url digest receipt="$media/claude-release.json"
    temporary=$(mktemp -d "$media/.claude-release.XXXXXXXX")
    trap 'rm -rf "$temporary"' EXIT
    fetch https://api.anthropic.com/api/desktop/darwin/universal/dmg/latest "$temporary/release.json" claude-release.json
    version=$(plutil -extract version raw -o - "$temporary/release.json")
    url=$(plutil -extract url raw -o - "$temporary/release.json")
    [[ $version =~ ^[0-9]+\.[0-9]+\.[0-9]+$ &&
       ${url%/*} == "https://downloads.claude.ai/releases/darwin/universal/$version" &&
       ${url##*/} =~ ^Claude-[a-f0-9]+\.dmg$ ]] || { echo 'Invalid official Claude Desktop release.' >&2; exit 1; }
    if [[ -f $receipt && ! -L $receipt && -f $media/claude.dmg && ! -L $media/claude.dmg ]] &&
        [[ $(plutil -extract version raw -o - "$receipt") == "$version" &&
           $(plutil -extract url raw -o - "$receipt") == "$url" &&
           $(plutil -extract sha256 raw -o - "$receipt") == "$(shasum -a 256 "$media/claude.dmg" | cut -d' ' -f1)" ]]; then
        bash "$src/claude-desktop-macos.sh" verify-media "$media/claude.dmg" "$version"
        exit 0
    fi
    printf '下载 claude.dmg\n'
    fetch "$url" "$temporary/claude.dmg" claude.dmg
    bash "$src/claude-desktop-macos.sh" verify-media "$temporary/claude.dmg" "$version"
    digest=$(shasum -a 256 "$temporary/claude.dmg" | cut -d' ' -f1)
    plutil -insert sha256 -string "$digest" "$temporary/release.json"
    [[ ! -L $media/claude.dmg && ! -L $receipt ]] || exit 1
    mv "$temporary/claude.dmg" "$media/claude.dmg"
    mv "$temporary/release.json" "$receipt"
)
download() {
    local name=$1 url=$2 expected=$3 api=${4:-}
    local target="$media/$name" temporary
    [[ ! -L $target && ( ! -e $target || -f $target ) ]]
    if [[ -f $target && ! -L $target ]]; then
        if [[ -z $expected && ${CAGE_REFRESH_TOOLS:-} != 1 ]] || [[ -n $expected && $(shasum -a 256 "$target" | cut -d' ' -f1) == "$expected" ]]; then return; fi
    fi
    printf '下载 %s\n' "$name"
    temporary=$(mktemp "$media/.$name.XXXXXXXX")
    if ! fetch "$url" "$temporary" "$name" "" "$expected"; then
        if [[ -z $api ]] || ! fetch "$api" "$temporary" "$name" application/octet-stream "$expected"; then
            rm "$temporary"
            return 1
        fi
    fi
    if [[ -n $expected && $(shasum -a 256 "$temporary" | cut -d' ' -f1) != "$expected" ]]; then
        rm "$temporary"
        return 1
    fi
    mv "$temporary" "$target"
}
download editor.tgz https://github.com/coder/code-server/releases/download/v4.145.0/code-server-4.145.0-macos-arm64.tar.gz a79774d8d34eed785c1f831bc0125f7534bc21a0039bef8e8a2aa32463f9421a https://api.github.com/repos/coder/code-server/releases/assets/711206948
download playwright.tgz https://registry.npmjs.org/playwright-core/-/playwright-core-1.65.0.tgz 315247c55a793ca760b2f05bdcee0e6a53fd65b1f22d1d3de8323c8c9395add3
download node.tgz https://nodejs.org/dist/v24.22.2/node-v24.22.2-darwin-arm64.tar.gz aa9549349e47dffaa42dcf95d6e06ade8c4bdafc980979883b88767670df385a
if [[ ${CAGE_REFRESH_TOOLS:-} != 1 ]]; then
    download macfuse.dmg https://github.com/macfuse/macfuse/releases/download/macfuse-5.5.0/macfuse-5.5.0.dmg 92c21ef2db4199bcf5bddc8e6efb57b23ecb9715f74ab38cba2c77ab226d3830
    if [[ ! -f $media/macfuse.pkg ]]; then bash "$src/macos-fuse-media.sh" "$media/macfuse.dmg" "$media/macfuse.pkg"; fi
    desktop_media
fi
download chrome.dmg https://dl.google.com/chrome/mac/universal/stable/GGRO/googlechrome.dmg ''
{
    fresh=$(mktemp -d "$media/.cli-repair.XXXXXXXX")
    if ! bash "$src/prepare-macos-cli-kit.sh" "$fresh/cli" "$media/cli"; then
        rmdir "$fresh"
        exit 1
    fi
    # Earlier builds cached a kit without Git. Preserve it until the full kit
    # is verified and the next install can resume after either rename.
    if [[ -d $media/cli ]]; then mv "$media/cli" "$fresh/previous"; fi
    mv "$fresh/cli" "$media/cli"
    rm -rf "$fresh"
}

# Cached archives may survive several application builds. Refresh first-party
# CLI source from this bundle without changing downloaded package bytes.
for name in install-macos-cli.sh macos-cli.py cli_channel.py cli_channel.resume.md claude_account_resume.md claude-account.py; do
    if [[ -f $src/$name ]]; then
        [[ ! -L $media/cli/$name ]]
        cp "$src/$name" "$media/cli/$name"
    fi
done
