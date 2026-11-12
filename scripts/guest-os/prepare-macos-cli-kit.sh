#!/bin/bash
# Prepare signed Claude Code and an offline uv Python distribution for a virtual Mac.
set -euo pipefail
[[ ( $# == 1 || $# == 2 ) && $1 == /* && ! -e $1 && ! -L $1 ]] || { echo 'Choose a new absolute CLI kit directory.' >&2; exit 2; }
cache=${2:-}
umask 077
src=$(cd "$(dirname "$0")" && pwd)
stage=$(mktemp -d "$(dirname "$1")/.cage-cli-kit.XXXXXXXX")
trap 'rm -rf "$stage"' EXIT
downloads=${CAGE_HOST_DOWNLOAD_CACHE:-$(dirname "$stage")/.downloads}
[[ ! -L $downloads ]] || exit 1
mkdir -p "$downloads"
fetch() {
    if [[ -n ${CAGE_HOST_DOWNLOADER:-} ]]; then
        if [[ -n ${5:-} ]]; then
            "$CAGE_HOST_DOWNLOADER" host-download "$1" "$2" "macos-cli-$3" "${4:-}" "$downloads" "$5"
        else
            "$CAGE_HOST_DOWNLOADER" host-download "$1" "$2" "macos-cli-$3" "${4:-}"
        fi
    else
        curl --disable --proto '=https' --proto-redir '=https' --location --fail --silent --show-error --connect-timeout 15 --speed-time 60 --speed-limit 1 --retry 2 -H "Accept: ${4:-*/*}" "$1" -o "$2"
    fi
}
download() {
    local name=$1 url=$2 digest=$3 api=$4
    local saved="$downloads/cli-$name"
    [[ ! -L $saved && ( ! -e $saved || -f $saved ) ]] || return 1
    # Retain completed siblings when one of the parallel downloads fails.
    # Hard links share storage with the published kit on the same cache volume.
    if [[ -f $saved ]] && ln "$saved" "$stage/$name"; then
        if [[ $(shasum -a 256 "$stage/$name" | cut -d' ' -f1) == "$digest" ]]; then return; fi
        rm "$stage/$name"
    fi
    if [[ -n $cache && -f $cache/$name && ! -L $cache/$name ]]; then
        cp "$cache/$name" "$stage/$name"
        if [[ $(shasum -a 256 "$stage/$name" | cut -d' ' -f1) == "$digest" ]]; then return; fi
    fi
    printf '下载 macos-cli-%s\n' "$name"
    if ! fetch "$url" "$stage/$name" "$name" "" "$digest"; then
        [[ -n $api ]] || return 1
        fetch "$api" "$stage/$name" "$name" application/octet-stream "$digest"
    fi
    [[ $(shasum -a 256 "$stage/$name" | cut -d' ' -f1) == "$digest" ]] || return 1
    ln -f "$stage/$name" "$saved"
}
releases=https://downloads.claude.ai/claude-code-releases
fetch "$releases/latest" "$stage/latest" latest
version=$(cat "$stage/latest")
[[ $version =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo 'Invalid official Claude Code version.' >&2; exit 1; }
fetch "$releases/$version/manifest.json" "$stage/claude-manifest.json" manifest.json
[[ $(plutil -extract version raw -o - "$stage/claude-manifest.json") == "$version" ]] || { echo 'Claude Code manifest version mismatch.' >&2; exit 1; }
digest=$(plutil -extract platforms.darwin-arm64.checksum raw -o - "$stage/claude-manifest.json")
[[ $digest =~ ^[a-f0-9]{64}$ ]] || exit 1
[[ $(plutil -extract platforms.darwin-arm64.binary raw -o - "$stage/claude-manifest.json") == claude ]] || exit 1
download claude "$releases/$version/darwin-arm64/claude" "$digest" '' &
claude_pid=$!
download uv.tar.gz https://github.com/astral-sh/uv/releases/download/0.13.7/uv-aarch64-apple-darwin.tar.gz 81b811b57e4624df7ed3aac148e0359100d9962f3d9dce00aec533dc2e086731 https://api.github.com/repos/astral-sh/uv/releases/assets/704009442 &
uv_pid=$!
download python.tar.gz https://github.com/astral-sh/python-build-standalone/releases/download/20261026/cpython-3.13.18%2B20261026-aarch64-apple-darwin-install_only_stripped.tar.gz 8392b6ea51d94391befb2c31ebe75463f045d0b69dfcf5e1bb3234e1f5fa3405 https://api.github.com/repos/astral-sh/python-build-standalone/releases/assets/678576053 &
python_pid=$!
download git.tar.gz https://github.com/desktop/dugite-native/releases/download/v2.56.0-2/dugite-native-v2.56.0-a44aeca-macOS-arm64.tar.gz 1110de343de11cc6f469b782b60f36217b9199b5e06875dd65baa26dd60563c7 https://api.github.com/repos/desktop/dugite-native/releases/assets/677615273 &
git_pid=$!
download gh.zip https://github.com/cli/cli/releases/download/v2.103.3/gh_2.103.3_macOS_arm64.zip d700207eae9ae98fe2b89535c2c087c07823d4628c56364695a5a07ef311c062 https://api.github.com/repos/cli/cli/releases/assets/679195704 &
gh_pid=$!
failed=0
for pid in "$claude_pid" "$uv_pid" "$python_pid" "$git_pid" "$gh_pid"; do wait "$pid" || failed=1; done
[[ $failed == 0 ]] || { echo 'Could not download and verify all macOS CLI packages.' >&2; exit 1; }
codesign --verify --strict --architecture arm64 -R '=anchor apple generic and identifier "com.anthropic.claude-code" and certificate leaf[subject.OU] = "Q6L2SF6YDW"' "$stage/claude"
rm "$stage/latest"
for file in internal/cage/claude-account.py internal/workbench/{cli_channel.py,cli_channel.resume.md,claude_account_resume.md}; do
    name=${file##*/}
    if [[ -f "$src/$name" ]]; then cp "$src/$name" "$stage/$name"; else cp "$src/../../$file" "$stage/$name"; fi
done
cp "$src"/{macos-cli.py,install-macos-cli.sh} "$stage/"
mv "$stage" "$1"
