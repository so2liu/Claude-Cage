#!/bin/bash
set -euo pipefail
mobile_dir="$(cd "$(dirname "$0")" && pwd)"
cd "$mobile_dir"
case "${1:-}" in
  android|ios) platform="$1" ;;
  android-release) platform=android ;;
  ios-ipa|ios-ipa-basic)
    platform=ios
    if [[ $# != 2 || ! $2 =~ ^[A-Z0-9]{10}$ ]]; then
      echo "Usage: mobile/build.sh $1 TEAM_ID (10 uppercase letters or digits)" >&2
      exit 2
    fi
    ;;
  *) echo "Usage: mobile/build.sh android | android-release | ios | ios-ipa TEAM_ID | ios-ipa-basic TEAM_ID" >&2; exit 2 ;;
esac
if [[ "$platform" == ios ]]; then
  # xcrun distinguishes a missing SDK from a pending Xcode license or setup step.
  xcrun --sdk iphoneos --show-sdk-path >/dev/null
fi
mkdir -p .build/bin ".build/$platform"
node build-ui.mjs
if [[ "$platform" == android ]]; then bash android/fetch-webkit.sh "$mobile_dir/.build/android"; fi
bash scripts/licenses.sh
(
  cd bindings
  go build -o ../.build/bin/gomobile golang.org/x/mobile/cmd/gomobile
  go build -o ../.build/bin/gobind golang.org/x/mobile/cmd/gobind
)
export PATH="$mobile_dir/.build/bin:$PATH"
if [[ -n "${JAVA_HOME:-}" ]]; then export PATH="$JAVA_HOME/bin:$PATH"; fi
if [[ "$platform" == ios ]]; then
  brand=".build/ios/Assets.xcassets/CageMark.imageset"
  mkdir -p "$brand"
  cp ../ui/src/logo.png "$brand/mark.png"
  printf '%s\n' '{"images":[{"filename":"mark.png","idiom":"universal"}],"info":{"author":"xcode","version":1}}' > "$brand/Contents.json"
  devices=".build/ios/Assets.xcassets/PairDevices.imageset"
  mkdir -p "$devices"
  cp assets/pair-devices.png "$devices/devices.png"
  sed 's/mark.png/devices.png/' "$brand/Contents.json" > "$devices/Contents.json"
  icon=".build/ios/Assets.xcassets/AppIcon.appiconset"
  mkdir -p "$icon"
  printf '%s\n' '{"info":{"author":"xcode","version":1}}' > ".build/ios/Assets.xcassets/Contents.json"
  cp ios/AppIcon.json "$icon/Contents.json"
  sips -z 1024 1024 ../ui/src/logo.png --out "$icon/Cage.png" >/dev/null
  # One arm64 iPhone slice and one simulator slice for this Mac.
  # Each platform framework is then passed to -create-xcframework exactly once.
  (cd bindings && gomobile bind -target="ios/arm64,iossimulator/$(go env GOHOSTARCH)" -iosversion=17.0 -o "$mobile_dir/.build/ios/Cagemobile.xcframework" .)
  if [[ "$1" == ios-ipa* ]]; then
    scheme=CageMobile
    if [[ "$1" == ios-ipa-basic ]]; then scheme=CageMobileBasic; fi
    output=$(mktemp -d "$mobile_dir/.build/ios/device-XXXXXXXX")
    xcodebuild -project ios/CageMobile.xcodeproj -scheme "$scheme" -configuration Release \
      -destination 'generic/platform=iOS' -archivePath "$output/CageMobile.xcarchive" \
      DEVELOPMENT_TEAM="$2" archive
    xcodebuild -exportArchive -archivePath "$output/CageMobile.xcarchive" \
      -exportOptionsPlist ios/DebuggingExport.plist -exportPath "$output/export"
    ipa="$output/export/$scheme.ipa"
    # Inspect the exported payload, which Xcode can re-sign after archiving.
    ditto -x -k "$ipa" "$output/check"
    app="$output/check/Payload/$scheme.app"
    test -s "$app/embedded.mobileprovision"
    codesign --verify --deep --strict "$app"
    rm -r "$output/check"
    printf 'Cage iPhone development IPA: %s\n' "$ipa"
  else
    xcodebuild -project ios/CageMobile.xcodeproj -target CageMobile -sdk iphoneos -configuration Debug \
      CODE_SIGNING_ALLOWED=NO SYMROOT="$mobile_dir/.build/ios/products" OBJROOT="$mobile_dir/.build/ios/obj" build
  fi
else
  : "${ANDROID_HOME:?Set ANDROID_HOME to the Android SDK directory}"
  (cd bindings && gomobile bind -target=android/arm64 -androidapi=26 \
    -ldflags='-extldflags=-Wl,-z,max-page-size=16384,-z,common-page-size=16384' \
    -o "$mobile_dir/.build/android/Cagemobile.aar" .)
  if [[ "$1" == android-release ]]; then
    bash android/package.sh --release
  else
    bash android/package.sh
  fi
fi
