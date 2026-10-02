#!/usr/bin/env bash
#
# Build the gateway, sign it with a *stable* identity, and restart the agent.
#
# Why signing is not optional: the gateway captures the module's USB Audio
# input, and macOS gates that behind microphone permission (TCC). An unsigned
# binary is identified by its contents, so every rebuild looks like a brand new
# app and macOS asks again — and while nobody has answered, CoreAudio does not
# fail, it hands back silence. That is indistinguishable from "the module is
# not delivering voice" and is one of the two ways a call ends up mute.
#
# Signing with a stable certificate keeps the TCC grant across rebuilds.
#
# usage: tools/deploy-gateway.sh [signing-identity]
set -euo pipefail

export PATH="${GOROOT_BIN:-$HOME/sdk/go/bin}:$PATH"

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
APP_DIR="${CELLBRIDGE_APP_DIR:-$HOME/Library/Application Support/CellBridge}"
# Signing identity: pass it as $1, set CELLBRIDGE_SIGN_IDENTITY, or let the
# script pick the first "Apple Development: …" identity in your keychain.
IDENTITY="${1:-${CELLBRIDGE_SIGN_IDENTITY:-}}"
if [ -z "$IDENTITY" ]; then
  IDENTITY="$(security find-identity -v -p codesigning 2>/dev/null \
    | sed -n 's/.*"\(Apple Development: [^"]*\)"/\1/p' | head -n 1)"
fi
LABEL="com.cellbridge.gateway"

cd "$REPO_DIR/mac-gateway"
go build -o "$APP_DIR/gatewayd" ./cmd/gatewayd

if [ -n "$IDENTITY" ] && security find-identity -v -p codesigning 2>/dev/null | grep -qF "$IDENTITY"; then
  codesign --force --sign "$IDENTITY" --identifier "$LABEL" --timestamp=none "$APP_DIR/gatewayd"
  echo "signed: $(codesign -dv --verbose=2 "$APP_DIR/gatewayd" 2>&1 | grep -E '^Identifier|^TeamIdentifier' | tr '\n' ' ')"
else
  echo "WARNING: signing identity not found, leaving the binary unsigned."
  echo "         macOS will re-ask for microphone permission after every rebuild,"
  echo "         and until it is granted the gateway captures silence."
  echo "         Fix: get an 'Apple Development' cert (Xcode > Settings > Accounts),"
  echo "         then re-run, or pass one explicitly: $0 \"Apple Development: you@example.com (XXXXXXXXXX)\""
fi

launchctl kickstart -k "gui/$UID/$LABEL"
sleep 4
curl -s -m 5 "localhost:8787/api/v1/status" && echo
echo "logged to /tmp/cellbridge-gateway.log"
