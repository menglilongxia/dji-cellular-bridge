#!/usr/bin/env bash
#
# Build, sign and install the CellBridge iOS app on this Mac.
#
# Why this script exists
# ----------------------
# The Mac is macOS 13.7 (Ventura), the iPhone is iOS 26.5.1.
#   * Xcode 15.4 (patched so its GUI can launch on Ventura) can *compile* for the
#     device and *sign* with a free Apple ID.
#   * Xcode 15.4's CoreDevice cannot talk to an iOS 26 device ("Device is busy
#     (Waiting to reconnect)"), so deployment goes through pymobiledevice3.
#   * /etc/hosts blocks Apple's TSS server (gs.apple.com -> 127.0.0.1) to stop
#     macOS updates; tools/tss-proxy.py re-resolves just that host over DoH so
#     pymobiledevice3 can fetch the DeveloperDiskImage. No sudo needed.
# See docs/IOS-BUILD-ON-VENTURA.md for the full write-up.
#
# Usage:
#   tools/build-ios.sh              # ddi + build + sign + install
#   tools/build-ios.sh ddi          # only mount the developer disk image
#   tools/build-ios.sh build        # build + sign, produce an .ipa
#   tools/build-ios.sh install      # install the .ipa that was built
#
# Env overrides: TEAM_ID, BUNDLE_ID, DEVICE_UDID, XCODE, WITH_PUSH=1
#
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
IOS_APP_DIR="$REPO_ROOT/ios-app"

# --- Discovered facts (2026-10-01) ----------------------------------------
# NOTE: TEAM_ID and DEVICE_UDID ship as placeholders. Override both, either via
# the environment or by editing them here, or the script refuses to build.
TEAM_ID="${TEAM_ID:-XXXXXXXXXX}"                    # your Apple Personal Team ID
BUNDLE_ID="${BUNDLE_ID:-com.example.cellbridge.sip}"    # must be unique/free on the portal
DEVICE_UDID="${DEVICE_UDID:-00008150-0000000000000000}"   # your iPhone's UDID
XCODE="${XCODE:-/Applications/Xcode.app}"
CONFIG="Debug"

require_configured() {
  local bad=0
  if [ "$TEAM_ID" = "XXXXXXXXXX" ]; then
    echo "error: TEAM_ID is still the placeholder." >&2
    echo "       find yours at https://developer.apple.com/account (Membership Details)," >&2
    echo "       or run: security find-identity -v -p codesigning" >&2
    bad=1
  fi
  if [ "$DEVICE_UDID" = "00008150-0000000000000000" ]; then
    echo "error: DEVICE_UDID is still the placeholder." >&2
    echo "       find it with: xcrun devicectl list devices   (or Xcode > Window > Devices)" >&2
    bad=1
  fi
  [ "$bad" = 0 ] || exit 2
}

# Free (Personal) teams cannot provision `aps-environment`, so the default build
# drops it and compiles PushKit out (iOS kills apps that register for VoIP push
# without the entitlement). WITH_PUSH=1 restores the paid-account behaviour.
WITH_PUSH="${WITH_PUSH:-0}"
if [ "$WITH_PUSH" = "1" ]; then
  ENTITLEMENTS="CellBridgeApp/CellBridgeApp.entitlements"
  SWIFT_CONDITIONS='$(inherited)'
else
  ENTITLEMENTS="CellBridgeApp/CellBridgeApp-free.entitlements"
  SWIFT_CONDITIONS='$(inherited) CELLBRIDGE_NO_PUSH'
fi

BUILD_DIR="$IOS_APP_DIR/build"
DERIVED="$BUILD_DIR/DerivedData"
APP_PATH="$DERIVED/Build/Products/$CONFIG-iphoneos/CellBridgeApp.app"
IPA_PATH="$BUILD_DIR/CellBridgeApp.ipa"

PYMOBILE="$HOME/Library/Python/3.9/bin/pymobiledevice3"
PYM_SITE="$HOME/Library/Python/3.9/lib/python/site-packages"
# ~/.pymobiledevice3 on this machine is root-owned (left by an older tool), so
# pymobiledevice3 is run with an isolated HOME. See the docs.
PMD3_HOME="$HOME/.pmd3home"
PROXY_PORT=8899

log() { printf '\n==> %s\n' "$*"; }

run_pmd3() {
  # PYTHONPATH must be a literal path: the shell would otherwise expand $HOME
  # against the overridden HOME below.
  HOME="$PMD3_HOME" \
  PYTHONPATH="$PYM_SITE" \
  PYMOBILEDEVICE3_UDID="$DEVICE_UDID" \
  HTTP_PROXY="http://127.0.0.1:$PROXY_PORT" \
  HTTPS_PROXY="http://127.0.0.1:$PROXY_PORT" \
    "$PYMOBILE" "$@"
}

ensure_proxy() {
  if nc -z 127.0.0.1 "$PROXY_PORT" 2>/dev/null; then
    return
  fi
  log "Starting tools/tss-proxy.py on 127.0.0.1:$PROXY_PORT"
  mkdir -p "$PMD3_HOME"
  nohup python3 "$REPO_ROOT/tools/tss-proxy.py" >"$BUILD_DIR/tss-proxy.log" 2>&1 &
  sleep 2
  nc -z 127.0.0.1 "$PROXY_PORT" 2>/dev/null || {
    echo "tss-proxy failed to start; see $BUILD_DIR/tss-proxy.log" >&2
    exit 1
  }
}

do_ddi() {
  ensure_proxy
  log "Verifying the proxy can reach Apple's TSS"
  curl -fsS --max-time 15 -x "http://127.0.0.1:$PROXY_PORT" -o /dev/null \
    "http://gs.apple.com/TSS/controller?action=2" \
    || { echo "proxy cannot reach gs.apple.com" >&2; exit 1; }

  log "Mounting the DeveloperDiskImage for the connected device"
  run_pmd3 mounter auto-mount
  run_pmd3 mounter list || true
}

do_build() {
  require_configured
  mkdir -p "$BUILD_DIR"
  log "Building + signing (team=$TEAM_ID bundle=$BUNDLE_ID)"
  log "entitlements=$ENTITLEMENTS  with_push=$WITH_PUSH"

  # -destination is required, not optional: it is what makes Xcode register the
  # device with the team (a profile can only be created once the UDID exists in
  # the team). It only works once the DeveloperDiskImage is mounted on the
  # device — see do_ddi. -derivedDataPath keeps the products inside the repo.
  xcodebuild \
    -project "$IOS_APP_DIR/CellBridgeApp.xcodeproj" \
    -scheme CellBridgeApp \
    -configuration "$CONFIG" \
    -destination "platform=iOS,id=$DEVICE_UDID" \
    -derivedDataPath "$DERIVED" \
    -allowProvisioningUpdates \
    -allowProvisioningDeviceRegistration \
    DEVELOPMENT_TEAM="$TEAM_ID" \
    CODE_SIGN_STYLE=Automatic \
    CODE_SIGN_ENTITLEMENTS="$ENTITLEMENTS" \
    PRODUCT_BUNDLE_IDENTIFIER="$BUNDLE_ID" \
    SWIFT_ACTIVE_COMPILATION_CONDITIONS="$SWIFT_CONDITIONS" \
    build

  log "Packaging IPA"
  rm -rf "$BUILD_DIR/Payload" "$IPA_PATH"
  mkdir -p "$BUILD_DIR/Payload"
  cp -R "$APP_PATH" "$BUILD_DIR/Payload/"
  (cd "$BUILD_DIR" && zip -qry "$(basename "$IPA_PATH")" Payload)
  log "IPA: $IPA_PATH"

  log "Signature"
  codesign -dv --verbose=2 "$APP_PATH" 2>&1 | sed -n '1,6p'
  codesign -d --entitlements - "$APP_PATH" 2>&1 | sed -n '1,12p' || true
}

do_install() {
  [ -f "$IPA_PATH" ] || { echo "no IPA at $IPA_PATH — run the build first" >&2; exit 1; }
  log "Installing onto the connected device"
  run_pmd3 apps install "$IPA_PATH"
  log "Launching $BUNDLE_ID"
  # Free-signed apps must be trusted once on the device
  # (Settings -> General -> VPN & Device Management -> Developer App -> Trust).
  # Until then launch fails with "Failed to launch process with bundle identifier".
  run_pmd3 developer core-device launch-application "$BUNDLE_ID" "" || {
    echo "Could not launch automatically — open the app manually on the iPhone." >&2
  }
}

case "${1:-all}" in
  ddi)     do_ddi ;;
  build)   do_build ;;
  install) do_install ;;
  all)     do_ddi; do_build; do_install ;;
  *)       echo "usage: $0 [all|ddi|build|install]" >&2; exit 2 ;;
esac
