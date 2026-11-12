# shellcheck shell=bash
# Functions are shared by first installation and an explicit in-place upgrade.
# Each invocation owns its staging files and leaves caller traps unchanged.
# Keep only version-shaped tokens; first-run notices can precede --version.
# Read all output so pipefail cannot turn an early reader exit into SIGPIPE.
cage_tool_version() {
  awk '$1 ~ /^v?[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$/ && !found { print $1; found=1 }'
}

# The caller holds the checksum directory lock. Partial files survive a failed
# attempt; only a complete checksum-verified artifact is eligible for install.
cage_download_tool() (
  set -euo pipefail
  local upstream="$1" checksum="$2" artifact="$3" hash="${4:-sha256sum}" mirror="${5:-}"
  local attempts="${6:-4}" attempt status http url part host
  if [ -f "$artifact" ]; then
    if printf '%s  %s\n' "$checksum" "$artifact" | "$hash" --check --status; then return 0; fi
    rm -f "$artifact"
  fi
  for ((attempt=1; attempt<=attempts; attempt++)); do
    url="$upstream"
    part="$artifact.part"
    if [ "$attempt" = 2 ] && [ -n "$mirror" ] && [ ! -s "$part" ]; then
      url="$mirror"
      part="$artifact.mirror.part"
    fi
    host="${url#https://}"
    printf 'Downloading %s from %s (attempt %s/%s; resume from %s bytes).\n' "${artifact##*/}" "${host%%/*}" "$attempt" "$attempts" "$(if [ -f "$part" ]; then wc -c < "$part"; else echo 0; fi)"
    if http="$(cage_curl_download "$part" --proto '=https' --proto-redir '=https' --tlsv1.2 --fail --location \
      --connect-timeout 15 \
      --continue-at - --write-out '%{http_code}' "$url" -o "$part" </dev/null)"; then
      status=0
    else
      status=$?
    fi
    # A previous attempt can have received the whole body before cancellation.
    if [ "$status" = 0 ] || [ "$http" = 416 ]; then
      if printf '%s  %s\n' "$checksum" "$part" | "$hash" --check --status; then
        mv -f "$part" "$artifact" || return 1
        rm -rf "$artifact.part.ranges" "$artifact.mirror.part.ranges"
        rm -f "$artifact.mirror.part"
        return 0
      fi
      rm -f "$part"
      rm -rf "$part.ranges"
      if [ "$url" != "$upstream" ]; then
        echo 'Mirror checksum mismatch; retrying from the official source.' >&2
        continue
      fi
      echo 'Tool checksum mismatch; installation refused. Retry downloads a fresh copy.' >&2
      return 1
    fi
    if [ "$url" != "$upstream" ]; then
      if [ "$http" = 404 ]; then
        echo 'This version is missing on the mirror; trying the official source.' >&2
      else
        echo 'Mirror download interrupted; trying the official source.' >&2
      fi
      continue
    fi
    if [ "$attempt" = 1 ] && [ -n "$mirror" ] && [ ! -s "$part" ]; then
      echo 'Official download unavailable; trying the fallback mirror.' >&2
      continue
    fi
    case "$status" in
      33|36) rm -f "$artifact.part" ;; # Server cannot resume: next attempt starts at zero.
      5|6|7|18|28|35|52|55|56|92) ;; # Connection, partial body, TLS EOF or timeout.
      22) case "$http" in 408|429|500|502|503|504) ;; *) return "$status" ;; esac ;;
      *) return "$status" ;;
    esac
    if [ "$attempt" != "$attempts" ]; then sleep "$attempt"; fi
  done
  echo 'Download interrupted; partial data is saved. Retry setup to continue.' >&2
  return "$status"
)

# npm caches verified tarballs by their integrity. Populate it as the same
# guest user that installs Claude, so a setup retry does not fetch its large
# optional native package again. npm remains responsible for dependency setup.
cage_cache_claude() (
  set -euo pipefail
  local arch="$1" wanted="$2" package metadata integrity checksum url cache
  case "$arch" in arm64) package=claude-code-linux-arm64 ;; amd64) package=claude-code-linux-x64 ;; esac
  metadata="$(npm view --registry=https://registry.npmjs.org "@anthropic-ai/$package@$wanted" dist --json </dev/null)"
  integrity="$(printf '%s' "$metadata" | jq -er '.integrity | select(startswith("sha512-"))')"
  checksum="$(printf '%s' "${integrity#sha512-}" | base64 -d | od -An -v -tx1 | tr -d ' \n')"
  [[ "$checksum" =~ ^[0-9a-f]{128}$ ]]
  url="$(printf '%s' "$metadata" | jq -er '.tarball | select(startswith("https://registry.npmjs.org/"))')"
  cache="$HOME/.cache/cage/downloads/$checksum"
  mkdir -p "$cache"
  exec 9>"$cache/lock"
  flock -w 60 9
  local mirror="https://cdn.npmmirror.com/packages/%40anthropic-ai/$package/$wanted/$package-$wanted.tgz"
  cage_download_tool "$url" "$checksum" "$cache/$package.tgz" sha512sum "$mirror"
  npm cache add --offline --ignore-scripts "$cache/$package.tgz" </dev/null
)

# Both archives contain the same release executable, but npm and GitHub
# package it differently. Keep their partials and checksum checks separate.
# The caller holds the original release's cache lock for the whole install.
cage_install_mise() (
  set -euo pipefail
  local wanted="$1" arch="$2" release_url="$3" release_sum="$4" cache="$5"
  local archive="$cache/mise.tar.gz" member=mise/bin/mise npm_arch=x64
  local npm_sum="$CAGE_MISE_NPM_AMD64_SHA256" npm_url npm_archive stage
  if [ "$arch" = arm64 ]; then
    npm_arch=arm64
    npm_sum="$CAGE_MISE_NPM_ARM64_SHA256"
  fi
  # The GitHub release is preferred. The independently pinned npm archive is
  # a fallback, including when the official connection cannot make progress.
  if ! cage_download_tool "$release_url" "$release_sum" "$archive" sha256sum '' 1; then
    npm_url="https://cdn.npmmirror.com/packages/%40jdxcode/mise-linux-$npm_arch/$wanted/mise-linux-$npm_arch-$wanted.tgz"
    npm_archive="$cache/$npm_sum/mise.tar.gz"
    mkdir -p "${npm_archive%/*}"
    if cage_download_tool "$npm_url" "$npm_sum" "$npm_archive" sha256sum '' 1; then
      archive="$npm_archive"
      member=package/bin/mise
    else
      echo 'mise mirror unavailable or failed verification; retrying the official release.' >&2
    fi
  fi
  if [ "$member" = mise/bin/mise ] && [ ! -f "$archive" ]; then
    cage_download_tool "$release_url" "$release_sum" "$archive" || return $?
  fi
  if [ "${CAGE_SETUP_PROGRESS:-}" = 1 ]; then echo 'Installing mise...'; fi
  mkdir -p "$HOME/.local/bin"
  stage="$(mktemp "$HOME/.local/bin/.mise-new-XXXXXX")"
  trap 'rm -f "$stage"' EXIT
  tar -xOzf "$archive" "$member" >"$stage" || return $?
  chmod 0755 "$stage" || return $?
  mv -f "$stage" "$HOME/.local/bin/mise"
)

cage_install_tool() (
  set -euo pipefail
  local tool="$1" arch current='' wanted url checksum artifact mirror=''
  arch="$(dpkg --print-architecture)"
  case "$arch" in arm64|amd64) ;; *) echo 'Cage tool packages support arm64 and amd64.' >&2; exit 1 ;; esac
  case "$tool" in
    gh)
      wanted="$CAGE_GH_VERSION"
      if command -v gh >/dev/null; then current="$(gh --version | awk '$1 == "gh" && $2 == "version" { print $3 }' | cage_tool_version)" || current=''; fi
      url="https://github.com/cli/cli/releases/download/v$wanted/gh_${wanted}_linux_${arch}.deb"
      if [ "$arch" = arm64 ]; then checksum="$CAGE_GH_ARM64_SHA256"; else checksum="$CAGE_GH_AMD64_SHA256"; fi
      artifact=gh.deb
      ;;
    code-server)
      wanted="$CAGE_CODE_SERVER_VERSION"
      if command -v code-server >/dev/null; then current="$(code-server --version | cage_tool_version)" || current=''; fi
      url="https://github.com/coder/code-server/releases/download/v$wanted/code-server_${wanted}_${arch}.deb"
      if [ "$arch" = arm64 ]; then checksum="$CAGE_CODE_SERVER_ARM64_SHA256"; else checksum="$CAGE_CODE_SERVER_AMD64_SHA256"; fi
      mirror="https://mirrors.ustc.edu.cn/github-release/coder/code-server/v$wanted/code-server_${wanted}_${arch}.deb"
      artifact=code-server.deb
      ;;
    mise)
      wanted="$CAGE_MISE_VERSION"
      if [ -x "$HOME/.local/bin/mise" ]; then current="$(MISE_DISABLE_UPDATE_WARNING=1 "$HOME/.local/bin/mise" --version | cage_tool_version)" || current=''; fi
      if [ "$arch" = arm64 ]; then checksum="$CAGE_MISE_ARM64_SHA256"; else checksum="$CAGE_MISE_AMD64_SHA256"; fi
      url="https://github.com/jdx/mise/releases/download/v$wanted/mise-v$wanted-linux-$(if [ "$arch" = arm64 ]; then echo arm64; else echo x64; fi).tar.gz"
      artifact=mise.tar.gz
      ;;
    claude)
      if [ "$arch" = amd64 ] && [ -r /proc/cpuinfo ] && ! grep -Eq '^flags[[:space:]]*:.*[[:space:]]sse4_2([[:space:]]|$)' /proc/cpuinfo; then
        echo 'Claude Code 需要 SSE4.2 指令。当前 x86_64 客机未提供该指令，请检查宿主 CPU、虚拟化平台和虚拟 CPU 配置后重启环境。' >&2
        exit 1
      fi
      # Resolve at installation/update time, then keep that exact version for
      # both the integrity-checked native cache and the npm installation.
      export npm_config_fetch_retries=3 npm_config_fetch_timeout=120000
      wanted="$(npm view --registry=https://registry.npmjs.org @anthropic-ai/claude-code@latest version --json </dev/null | jq -er 'select(type == "string") | select(test("^[0-9]+\\.[0-9]+\\.[0-9]+$"))')"
      if command -v claude >/dev/null; then current="$(DISABLE_AUTOUPDATER=1 claude --version | cage_tool_version)" || current=''; fi
      ;;
    *) echo 'Unknown Cage tool.' >&2; exit 2 ;;
  esac
  if [ -n "$current" ] && dpkg --compare-versions "${current#v}" ge "$wanted"; then
    printf '%s already meets required version %s.\n' "$tool" "$wanted"
    exit 0
  fi
  if [ "$tool" = claude ]; then
    export npm_config_fetch_retries=3 npm_config_fetch_timeout=120000
    cage_cache_claude "$arch" "$wanted"
    if [ "${CAGE_SETUP_PROGRESS:-}" = 1 ]; then echo "Installing Claude Code..."; fi
    npm install --registry=https://registry.npmjs.org --prefix "$HOME/.local" --global "@anthropic-ai/claude-code@$wanted" </dev/null
    exit 0
  fi
  local cache="$HOME/.cache/cage/downloads/$checksum"
  mkdir -p "$cache"
  # First setup and maintenance can both reach this helper in a Linux guest.
  exec 9>"$cache/lock"
  flock -w 60 9
  if [ "$tool" = mise ]; then
    cage_install_mise "$wanted" "$arch" "$url" "$checksum" "$cache"
    exit $?
  fi
  cage_download_tool "$url" "$checksum" "$cache/$artifact" sha256sum "$mirror"
  if [ "${CAGE_SETUP_PROGRESS:-}" = 1 ]; then printf 'Installing %s...\n' "$tool"; fi
  sudo env DEBIAN_FRONTEND=noninteractive APT_CONFIG="${CAGE_APT_CONFIG:-/dev/null}" apt-get -o Acquire::Retries=3 install -y --no-install-recommends "$cache/$artifact" </dev/null
)
