#!/usr/bin/env bash
# Builds the debug APK inside a JDK container. The container gives Gradle a clean
# loopback (some hosts NAT 127.0.0.1, which breaks the Gradle daemon handshake).
# On linux/arm64, replace build-tools/35.0.0/{aapt,aapt2,zipalign} with aarch64
# builds (github.com/lzhiyong/android-sdk-tools) since Google ships x86_64 only.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SDK="${ANDROID_SDK_ROOT:-${ANDROID_HOME:-$HOME/Android/Sdk}}"
if [ ! -d "$SDK/platforms/android-35" ]; then
  echo "android sdk missing platforms;android-35 under $SDK" >&2
  exit 1
fi
printf 'sdk.dir=/sdk\n' > "$ROOT/apps/android/local.properties"
if [ "$(uname -m)" = "aarch64" ]; then
  AAPT2_OVERRIDE="${AAPT2_OVERRIDE:-/sdk/build-tools/35.0.0/aapt2}"
fi
docker run --rm \
  -u "$(id -u):$(id -g)" \
  -v "$SDK:/sdk" \
  -v "$ROOT/apps/android:/src" \
  -e ANDROID_HOME=/sdk \
  -e ANDROID_SDK_ROOT=/sdk \
  -e GRADLE_USER_HOME=/src/.gradle-home \
  -e HOME=/src/.gradle-home \
  -e AAPT2_OVERRIDE="${AAPT2_OVERRIDE:-}" \
  -w /src \
  eclipse-temurin:17-jdk \
  bash -lc './gradlew :app:assembleDebug --no-daemon ${AAPT2_OVERRIDE:+-Pandroid.aapt2FromMavenOverride=$AAPT2_OVERRIDE}'
ls -l "$ROOT/apps/android/app/build/outputs/apk/debug/app-debug.apk"
