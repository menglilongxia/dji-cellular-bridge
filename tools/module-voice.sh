#!/usr/bin/env bash
#
# Bring the module-side VoLTE voice route up / down on a DJI QDC507.
#
# The gateway runs this when a call becomes active. It is the piece that makes
# call audio actually flow (verified 2026-10-01):
#
#   1. kernel modules  qdc507_aprv3.ko + qdc507_voice.ko  (insmod once per boot)
#   2. ALSA calibration via /usr/bin/alsaucm_test: open the Tomtom I2S use case,
#      select the VoLTE verb and enable the Auxpcm Rx/Tx devices. Without this
#      the VoLTE PCM never reaches the UAC and the host hears silence.
#   3. mavo-pcm-bridge --voice-route-session — forces a fresh route every call
#      (CellBridge documents a UAC firmware bug where the route dies after a
#      playback open, hence the rotate).
#
# The files and the ADB tool live outside the repo:
#   module side : /tmp/mavo-call/{qdc507_aprv3.ko,qdc507_voice.ko,mavo-pcm-bridge.armv7}
#   mac side    : qdc507-push (mac-gateway/cmd/qdc507-push) + libusb
#
# usage: module-voice.sh start|stop|status
set -euo pipefail

QDC507_PUSH="${QDC507_PUSH:-/tmp/qdc507-push}"
export DYLD_LIBRARY_PATH="${DYLD_LIBRARY_PATH:-/tmp/libusb-prefix/lib}"

# Where the vendor runtime lives on this Mac, and how to reach the module.
SRC_DIR="${SRC_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)/work/module-voice}"
MODULE_IP="${MODULE_IP:-192.168.225.1}"
FILES=(qdc507_aprv3.ko qdc507_voice.ko mavo-pcm-bridge.armv7)

adb_shell() {
  "$QDC507_PUSH" -state shell "$1" 2>/dev/null | tail -n +1 || true
}

DIR=/tmp/mavo-call

# push_file <name> <port>  — the module listens, the Mac connects.
# The module cannot reach the Mac (its firewall drops outbound to the host),
# but Mac -> module is fine, so the module is the listener.
push_file() {
  local name="$1" port="$2"
  "$QDC507_PUSH" -state shell \
    "mkdir -p $DIR; cd $DIR && busybox nc -l -p $port > $name" >/dev/null 2>&1 &
  sleep 2
  nc -w 20 "$MODULE_IP" "$port" < "$SRC_DIR/$name" || true
  sleep 1
}

ensure_files() {
  for f in "${FILES[@]}"; do
    if adb_shell "test -s $DIR/$f && echo FILE-OK" | grep -q FILE-OK; then
      continue
    fi
    echo "pushing $f"
    case "$f" in
      *armv7) push_file "$f" 9101 ;;
      *aprv3*) push_file "$f" 9102 ;;
      *)       push_file "$f" 9103 ;;
    esac
    adb_shell "chmod 755 $DIR/mavo-pcm-bridge.armv7"
  done
}

# Each step is a separate, simple shell command: a single giant inline script
# is fragile to quote through cgo -> libusb -> adbd -> busybox ash.
voice_start() {
  adb_shell "lsmod | grep -q '^qdc507_aprv3 ' || insmod $DIR/qdc507_aprv3.ko"
  adb_shell "lsmod | grep -q '^qdc507_voice '  || insmod $DIR/qdc507_voice.ko"
  # rotate: a stale bridge / calibrator breaks the next call
  # rotate only if nothing is running yet
  adb_shell "if ps | grep -q '[m]avo'; then echo already-running; else pkill -f alsaucm_test 2>/dev/null; rm -f /run/alsaucm_test; echo rotated; fi"
  # ALSA/ACDB calibration for the VoLTE use case
  adb_shell "setsid /usr/bin/alsaucm_test </dev/null > /tmp/cal.log 2>&1 & sleep 3; test -p /run/alsaucm_test && echo FIFO-READY || echo NO-FIFO"
  adb_shell "printf 'open snd_soc_msm_9x07_Tomtom_I2S\\n' > /run/alsaucm_test"
  adb_shell "printf 'set _verb VoLTE\\n' > /run/alsaucm_test"
  adb_shell "printf 'set _enadev Auxpcm Rx\\n' > /run/alsaucm_test"
  adb_shell "printf 'set _enadev Auxpcm Tx\\n' > /run/alsaucm_test"
  # The ACDB calibration takes a couple of seconds; poll instead of a fixed sleep.
  adb_shell "n=0; while [ \$n -lt 40 ]; do grep -q 'Sent VocProc Cal' /tmp/cal.log 2>/dev/null && break; sleep 0.25; n=\$((n+1)); done; grep -c 'Sent VocProc Cal' /tmp/cal.log"
  adb_shell "setsid $DIR/mavo-pcm-bridge.armv7 --voice-route-session > /tmp/mavo-route.log 2>&1 & sleep 2; ps | grep -c '[m]avo'; grep -c 'route session active' /tmp/mavo-route.log"
}

stop_script=$(cat <<'EOS'
pkill -f mavo-pcm-bridge 2>/dev/null
echo stopped
EOS
)

case "${1:-start}" in
  provision) ensure_files; voice_start ;;
  start)     ensure_files; voice_start ;;
  stop)      adb_shell "$stop_script" ;;
  status)    adb_shell "lsmod | grep qdc507; ls /dev/snd | tr '\n' ' '; ps | grep -c '[m]avo'" ;;
  *)         echo "usage: $0 start|stop|status" >&2; exit 2 ;;
esac
