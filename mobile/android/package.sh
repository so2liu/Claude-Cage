#!/bin/bash
# Assemble the local AAR and bundled UI into a signed APK.
set -euo pipefail
mobile_dir="$(cd "$(dirname "$0")/.." && pwd)"
android="$mobile_dir/android"
build="$mobile_dir/.build/android"
aar="$build/Cagemobile.aar"
sdk="${ANDROID_HOME:?Set ANDROID_HOME to the Android SDK directory}"
tools="$sdk/build-tools/${CAGE_BUILD_TOOLS:-35.0.0}"
platform="$sdk/platforms/android-${CAGE_ANDROID_PLATFORM:-35}/android.jar"
if [[ -n "${JAVA_HOME:-}" ]]; then export PATH="$JAVA_HOME/bin:$PATH"; fi
version_args=()
signing_args=()
case "${1:-}" in
  '') out="$build/cage-mobile-debug.apk" ;;
  --release)
    : "${CAGE_ANDROID_KEYSTORE:?Set the persistent Android signing keystore path}"
    : "${CAGE_ANDROID_KEY_ALIAS:?Set the signing key alias}"
    : "${CAGE_ANDROID_PASSWORD_FILE:?Set the private keystore password file path}"
    : "${CAGE_ANDROID_VERSION_CODE:?Set an Android version code greater than the previous release}"
    [[ "$CAGE_ANDROID_VERSION_CODE" =~ ^[1-9][0-9]{0,9}$ && "$CAGE_ANDROID_VERSION_CODE" -le 2100000000 ]] || { echo 'Invalid Android version code.' >&2; exit 2; }
    [[ -f "$CAGE_ANDROID_KEYSTORE" && -s "$CAGE_ANDROID_PASSWORD_FILE" ]] || { echo 'Android signing material is unavailable.' >&2; exit 1; }
    version="$(tr -d '\r\n' < "$mobile_dir/../VERSION")"
    [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || { echo 'Invalid application VERSION.' >&2; exit 2; }
    version_args=(--replace-version --version-name "$version" --version-code "$CAGE_ANDROID_VERSION_CODE")
    signing_args=(--ks "$CAGE_ANDROID_KEYSTORE" --ks-key-alias "$CAGE_ANDROID_KEY_ALIAS" --ks-pass "file:$CAGE_ANDROID_PASSWORD_FILE")
    out="$build/Cage-$version-android-arm64.apk"
    ;;
  *) echo 'Usage: mobile/android/package.sh [--release]' >&2; exit 2 ;;
esac
[[ -f "$aar" && -f "$mobile_dir/.build/resources/mobile-ui/mobile.html" && -f "$platform" ]] || { echo 'Missing native AAR, mobile UI, or Android platform.' >&2; exit 1; }
mkdir -p "$build"
work="$(mktemp -d "$build/package.XXXXXX")"
trap 'rm -rf "$work"' EXIT
mkdir -p "$work/classes" "$work/dex" "$work/aar" "$work/apk/lib/arm64-v8a"
mkdir -p "$work/brand/drawable-nodpi"
cp "$mobile_dir/assets/pair-devices.png" "$work/brand/drawable-nodpi/pair_devices.png"
cp "$mobile_dir/../ui/src/logo.png" "$work/brand/drawable-nodpi/cage_icon.png"
unzip -q "$aar" -d "$work/aar"
cp "$work/aar/jni/arm64-v8a/libgojni.so" "$work/apk/lib/arm64-v8a/"
mkdir -p "$work/apk/assets/licenses"
cp "$android/licenses/ZXing-LICENSE.txt" "$work/apk/assets/licenses/"
"$tools/aapt2" compile --dir "$android/res" -o "$work/res.zip"
"$tools/aapt2" compile --dir "$work/brand" -o "$work/brand.zip"
"$tools/aapt2" link -o "$work/base.apk" -I "$platform" -A "$mobile_dir/.build/resources" --manifest "$android/AndroidManifest.xml" "${version_args[@]}" "$work/res.zip" "$work/brand.zip"
zxing="$build/zxing-core-3.5.3.jar"
if [[ ! -f "$zxing" ]]; then
  curl --fail --location --silent --show-error https://repo.maven.apache.org/maven2/com/google/zxing/core/3.5.3/core-3.5.3.jar -o "$work/zxing.jar"
  cp "$work/zxing.jar" "$zxing"
fi
echo "8d8064c1636fdaef7189dd9055c7d59950a8940a12f2293956446ec3c109fd82  $zxing" | shasum -a 256 --check --status
bash "$android/fetch-webkit.sh" "$build"
javac --release 8 -encoding UTF-8 -classpath "$platform:$work/aar/classes.jar:$zxing:$build/webkit-1.14.0.jar:$build/core-1.1.0.jar" -d "$work/classes" "$android"/src/io/github/leexeo/cage/mobile/*.java
jar cf "$work/app.jar" -C "$work/classes" .
"$tools/d8" --min-api 26 --lib "$platform" --output "$work/dex" "$work/app.jar" "$work/aar/classes.jar" "$zxing" "$build/webkit-1.14.0.jar" "$build/core-1.1.0.jar"
cp "$work/dex/"*.dex "$work/apk/"
(cd "$work/apk" && zip -q -0 "$work/base.apk" ./*.dex lib/arm64-v8a/libgojni.so assets/licenses/ZXing-LICENSE.txt)
"$tools/zipalign" -f -P 16 4 "$work/base.apk" "$work/aligned.apk"
if [[ "${1:-}" != --release ]]; then
  key="$build/development.p12"
  if [[ ! -f "$key" ]]; then
    (umask 077; keytool -genkeypair -alias development -keyalg RSA -keysize 2048 -validity 3650 \
      -keystore "$key" -storetype PKCS12 -storepass android -keypass android -dname "CN=Cage local development")
  fi
  signing_args=(--ks "$key" --ks-key-alias development --ks-pass pass:android)
fi
"$tools/apksigner" sign "${signing_args[@]}" --out "$work/signed.apk" "$work/aligned.apk"
"$tools/apksigner" verify --verbose "$work/signed.apk"
"$tools/zipalign" -c -P 16 4 "$work/signed.apk"
mv "$work/signed.apk" "$out"
echo "$out"
