#!/bin/bash
# Bundle notices for the linked mobile dependencies and bundled interface.
set -euo pipefail
mobile_dir="$(cd "$(dirname "$0")/.." && pwd)"
cd "$mobile_dir/bindings"
output="$mobile_dir/.build/resources/licenses"
mkdir -p "$output"
list="$(mktemp)"
trap 'rm -f "$list"' EXIT
# Both gomobile targets share the bundled notices.
for target in android ios; do
  GOOS="$target" GOARCH=arm64 CGO_ENABLED=1 go list -deps -f '{{with .Module}}{{.Path}}{{"\t"}}{{.Version}}{{"\t"}}{{.Dir}}{{end}}' .
done | awk -F '\t' 'NF == 3 && !seen[$1]++' | LC_ALL=C sort > "$list"
{
  cat <<'NOTICE'
Cage mobile third-party notices

Cage mobile source and build scripts: https://github.com/Lee-xeo/claude-cage
The licenses of the linked Go modules are included below.

NOTICE
  go_license="$(go env GOROOT)/LICENSE"
  [[ -f "$go_license" ]] || go_license="$(go env GOROOT)/../LICENSE"
  cat "$go_license"
  while IFS=$'\t' read -r path version directory; do
    for file in "$directory"/LICENSE* "$directory"/LICENCE* "$directory"/COPYING* "$directory"/NOTICE* "$directory"/UNLICENSE*; do
      [[ -f "$file" ]] || continue
      printf '\n======== %s %s: %s ========\n\n' "$path" "$version" "$(basename "$file")"
      cat "$file"
    done
  done < "$list"
} > "$output/THIRD_PARTY_NOTICES.txt"
for package in react react-dom scheduler lucide-react; do
  cp "$mobile_dir/../ui/node_modules/$package/LICENSE" "$output/$package-LICENSE.txt"
done
cp "$mobile_dir/../ui/node_modules/marked/LICENSE" "$output/Marked-LICENSE.txt"
cp "$mobile_dir/android/licenses/ZXing-LICENSE.txt" "$output/ZXing-LICENSE.txt"
if [[ -f "$mobile_dir/.build/android/webkit-1.14.0.aar" ]]; then
  unzip -p "$mobile_dir/.build/android/webkit-1.14.0.aar" META-INF/androidx/webkit/webkit/LICENSE.txt > "$output/AndroidX-LICENSE.txt"
fi
