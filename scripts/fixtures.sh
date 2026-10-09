#!/usr/bin/env bash
# Regenerates the APK test fixtures in internal/apk/testdata/.
#
# Requires the Android SDK tools (aapt2, zipalign, apksigner from build-tools)
# and a JDK (keytool). Override with:
#   ANDROID_SDK_ROOT=/path/to/android-sdk  JAVA_HOME=/path/to/jdk
#
# Fixtures produced:
#   v2only.apk    - baseline, signed with the test key using v2 scheme only
#   v2v3.apk      - same, signed with v2+v3
#   wrongkey.apk  - signed with an unrelated ("evil") key
#   v1only.apk    - signed with the v1 (JAR) scheme only
#   tampered.apk  - v2only.apk with one content byte flipped
set -euo pipefail

SDK="${ANDROID_SDK_ROOT:-/home/bombo/toolchain/android-sdk}"
JAVA_HOME="${JAVA_HOME:-/home/bombo/toolchain/jdk17}"
export PATH="$JAVA_HOME/bin:$PATH"
BT="$SDK/build-tools/34.0.0"
ANDROID_JAR="$SDK/platforms/android-34/android.jar"

HERE="$(cd "$(dirname "$0")/.." && pwd)"
OUT="$HERE/internal/apk/testdata"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

KEYTOOL="$JAVA_HOME/bin/keytool"
AAPT2="$BT/aapt2"
ZIPALIGN="$BT/zipalign"
APKSIGNER="$BT/apksigner"
for t in "$KEYTOOL" "$AAPT2" "$ZIPALIGN" "$APKSIGNER" "$ANDROID_JAR"; do
  if [ ! -x "$t" ] && [ ! -f "$t" ]; then
    echo "missing required tool: $t" >&2
    exit 1
  fi
done

cat > "$WORK/AndroidManifest.xml" <<'EOF'
<?xml version="1.0" encoding="utf-8"?>
<manifest xmlns:android="http://schemas.android.com/apk/res/android"
    package="com.roblox.client"
    android:versionCode="2734"
    android:versionName="2.734.917">
    <uses-sdk android:minSdkVersion="21" android:targetSdkVersion="34"/>
    <application android:label="rbxrt test fixture"/>
</manifest>
EOF
echo "matching rob 934 b545 a7f8 c2d4 e619 f0a3 117b 8e42 7a05 c986" > "$WORK/entry.txt"

echo "==> building base APK with aapt2"
"$AAPT2" link \
  -I "$ANDROID_JAR" \
  --manifest "$WORK/AndroidManifest.xml" \
  --min-sdk-version 21 \
  --target-sdk-version 34 \
  -o "$WORK/base.apk"

echo "==> adding a second entry (entry.txt)"
(cd "$WORK" && zip -q -X base.apk entry.txt)

for name in test evil; do
  ks="$OUT/keys/$name.jks"
  if [ -f "$ks" ]; then
    echo "==> reusing $name keystore ($ks)"
  else
    echo "==> generating $name keystore (stored at $ks)"
    mkdir -p "$OUT/keys"
    "$KEYTOOL" -genkeypair -noprompt \
      -keystore "$ks" -storepass password -keypass password \
      -alias "$name" -keyalg RSA -keysize 2048 -validity 10000 \
      -dname "CN=rbxrt $name cert, OU=rbxrt, O=rbxrt, C=US"
  fi
done

sign() { # out key v2 v1 [v3]
  local out="$1" key="$2" v2="$3" v1="$4" v3="${5:-false}"
  "$APKSIGNER" sign \
    --ks "$OUT/keys/$key.jks" --ks-pass pass:password --key-pass pass:password \
    --v1-signing-enabled "$v1" --v2-signing-enabled "$v2" \
    --v3-signing-enabled "$v3" --v4-signing-enabled false \
    --min-sdk-version 24 \
    --out "$OUT/$out" \
    "$WORK/pre.apk"
}

echo "==> aligning and signing fixtures"
"$ZIPALIGN" -f 4 "$WORK/base.apk" "$WORK/pre.apk"
sign v2only.apk test true false
sign v2v3.apk test true false true
sign wrongkey.apk evil true false

echo "==> building v1-only fixture"
"$ZIPALIGN" -f 4 "$WORK/base.apk" "$WORK/pre.apk"
"$APKSIGNER" sign \
  --ks "$OUT/keys/test.jks" --ks-pass pass:password --key-pass pass:password \
  --v1-signing-enabled true --v2-signing-enabled false --v3-signing-enabled false \
  --min-sdk-version 21 \
  --out "$OUT/v1only.apk" \
  "$WORK/pre.apk"

echo "==> building tampered fixture (flip byte at offset 50)"
cp "$OUT/v2only.apk" "$WORK/tampered.apk"
printf '\x41' | dd of="$WORK/tampered.apk" bs=1 seek=50 conv=notrunc status=none
cp "$WORK/tampered.apk" "$OUT/tampered.apk"

echo "==> verifying fixtures"
for f in v2only v2v3 wrongkey v1only; do
  if "$APKSIGNER" verify --min-sdk-version 24 "$OUT/$f.apk" 2>/dev/null; then
    echo "  $f: verifies (scheme OK)"
  else
    echo "  $f: DOES NOT VERIFY"
    "$APKSIGNER" verify --min-sdk-version 24 "$OUT/$f.apk" || true
  fi
done

echo "==> cert SHAs (used by verify_test.go)"
for pair in "test:v2only" "evil:wrongkey"; do
  k="${pair%%:*}" f="${pair##*:}"
  sha="$("$APKSIGNER" verify --print-certs --min-sdk-version 24 "$OUT/$f.apk" 2>/dev/null | grep -m1 'SHA-256 digest:' | awk '{print $NF}')"
  echo "  $k: $sha"
done

echo "done -> $OUT"
ls -la "$OUT"