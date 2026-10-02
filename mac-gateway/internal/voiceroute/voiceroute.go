// Package voiceroute brings up the module-side VoLTE voice path.
//
// Without it a call connects but carries silence: the QDC507 keeps the
// cellular voice on an internal PCM bus until the right kernel modules are
// loaded, the ALSA/ACDB use case is calibrated, and mavo-pcm-bridge is asked
// for a voice-route session.
//
// All of that is done over ADB, and the ADB session now lives in the gateway
// process (see internal/adb). That is the whole point of this package: no
// second process may open the USB device, because doing so seizes the pipes
// the gateway's AT channel is using and wedges the module's adbd.
//
// The sequence is deliberately short — a call rings for only a few seconds and
// provisioning takes 7–15 s, so it has to start at the first RING:
//
//  1. make sure the vendor runtime is on the module (netcat push, verified by
//     md5 — the module cannot reach the Mac, so the module listens and we dial)
//  2. insmod qdc507_aprv3.ko + qdc507_voice.ko (once per module boot)
//  3. start /usr/bin/alsaucm_test and drive it through the VoLTE verb
//  4. start mavo-pcm-bridge.armv7 --voice-route-session
package voiceroute

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dji-cellular-bridge/mac-gateway/internal/adb"
)

const (
	// DefaultRemoteDir is where the vendor runtime lives on the module.
	DefaultRemoteDir = "/tmp/mavo-call"
	// DefaultModuleIP is the module's address on its USB ECM link; the Mac is
	// 192.168.225.20 and only Mac -> module connections get through.
	DefaultModuleIP = "192.168.225.1"

	aprvModule  = "qdc507_aprv3.ko"
	voiceModule = "qdc507_voice.ko"
	bridgeBin   = "mavo-pcm-bridge.armv7"

	calLog   = "/tmp/cal.log"
	routeLog = "/tmp/mavo-route.log"
	calFIFO  = "/run/alsaucm_test"

	// Pid files let us stop the previous call's calibrator and bridge without
	// "pkill -f": that pattern would match the provisioning script's own
	// command line, and the shell running it would kill itself mid-sequence.
	calPidFile    = "/tmp/cb-cal.pid"
	bridgePidFile = "/tmp/cb-bridge.pid"

	// calibrator is the vendor tool that applies the ACDB calibration for the
	// VoLTE use case. Without the two mixer controls it writes, the cellular
	// PCM never reaches the UAC interface and the Mac hears nothing.
	calibrator = "/usr/bin/alsaucm_test"
	// calRxMarker/calTxMarker are the lines that prove the calibration ran.
	// CellBridge (verified on this module) polls for the ACDB line; the mixer
	// line is the fallback our firmware also prints.
	calRxMarker = "ACDB -> Sent VocProc Cal!"
	// routeReadyMarker is what the helper logs once the PCM path is attached.
	// CellBridge's readiness script greps exactly this string.
	routeReadyMarker = "VoLTE route session active on hw:0,4"

	pushPortBase = 9101
	pushAttempts = 3

	// provisionTimeout covers the whole module-side script: loading modules,
	// waiting for the calibrator's FIFO, the ACDB calibration and starting the
	// PCM bridge. Everything inside the script is bounded, so this is a
	// backstop rather than the normal path.
	provisionTimeout = 90 * time.Second
)

type runtimeFile struct {
	name string
	mode int
}

// Order matters only in that the bridge is started last.
var runtimeFiles = []runtimeFile{
	{aprvModule, 0o644},
	{voiceModule, 0o644},
	{bridgeBin, 0o755},
}

// Options configures a Provisioner.
type Options struct {
	// ADB is the in-process ADB client owned by the modem driver.
	ADB *adb.Client
	// FilesDir holds the vendor runtime downloaded from moluncn/mavo.
	FilesDir string
	// ModuleIP is the module's address on the USB ECM link.
	ModuleIP string
	// RemoteDir is the module-side directory to install into.
	RemoteDir string
	// Logf receives progress lines. Defaults to no logging.
	Logf func(format string, args ...any)
	// Dial is used for the netcat push; tests replace it.
	Dial func(ctx context.Context, network, address string) (net.Conn, error)
}

// Result describes one provisioning attempt.
type Result struct {
	At            time.Time     `json:"at"`
	Duration      time.Duration `json:"duration_ms"`
	Pushed        []string      `json:"pushed,omitempty"`
	ModulesLoaded bool          `json:"modules_loaded"`
	Calibrated    bool          `json:"calibrated"`
	BridgeRunning bool          `json:"bridge_running"`
	Err           error         `json:"-"`
}

// ErrBusy is returned when a provisioning run is already in flight.
var ErrBusy = errors.New("voiceroute: provisioning already in progress")

// Provisioner runs the module-side voice provisioning sequence. Use one per
// module; it serialises runs and remembers the last result.
type Provisioner struct {
	opts Options

	mu       sync.Mutex
	inflight chan struct{} // closed when the current run finishes
	last     Result
}

// New builds a Provisioner. ADB must be non-nil.
func New(opts Options) *Provisioner {
	if opts.RemoteDir == "" {
		opts.RemoteDir = DefaultRemoteDir
	}
	if opts.ModuleIP == "" {
		opts.ModuleIP = DefaultModuleIP
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...any) {}
	}
	if opts.Dial == nil {
		d := &net.Dialer{Timeout: 3 * time.Second}
		opts.Dial = func(ctx context.Context, network, address string) (net.Conn, error) {
			return d.DialContext(ctx, network, address)
		}
	}
	return &Provisioner{opts: opts}
}

// Last returns the most recent result.
func (p *Provisioner) Last() Result {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.last
}

// Running reports whether a run is in flight.
func (p *Provisioner) Running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.inflight != nil
}

// Start provisions the voice route. Concurrent calls join the run already in
// flight instead of starting a second one (a second run would kill the first
// one's calibrator halfway through).
func (p *Provisioner) Start(ctx context.Context) (Result, error) {
	p.mu.Lock()
	if p.inflight != nil {
		done := p.inflight
		p.mu.Unlock()
		select {
		case <-done:
			return p.Last(), p.Last().Err
		case <-ctx.Done():
			return p.Last(), ctx.Err()
		}
	}
	done := make(chan struct{})
	p.inflight = done
	p.mu.Unlock()

	started := time.Now()
	res := Result{At: started}
	err := p.run(ctx, &res)
	res.Duration = time.Since(started)
	res.Err = err

	p.mu.Lock()
	p.last = res
	p.inflight = nil
	p.mu.Unlock()
	close(done)
	return res, err
}

// Stop tears the voice route down (call ended). It is safe to call when
// nothing is running.
func (p *Provisioner) Stop(ctx context.Context) error {
	// By pid file rather than "pkill -f mavo-pcm-bridge": the pattern would
	// match this command's own shell, which would kill itself.
	_, err := p.shell(ctx, fmt.Sprintf(
		"[ -f %[1]s ] && kill $(cat %[1]s) 2>/dev/null; rm -f %[1]s; echo stopped", bridgePidFile),
		10*time.Second)
	return err
}

func (p *Provisioner) run(ctx context.Context, res *Result) error {
	p.logf("provision: start")
	if p.opts.ADB == nil {
		return errors.New("voiceroute: no adb client")
	}
	if err := p.opts.ADB.EnsureConnected(ctx); err != nil {
		return err
	}
	// One shell invocation per attempt: every ADB shell costs this 43 MB module
	// a fork, and forks are what it runs out of, so the whole sequence runs as a
	// single script that does its waiting on the module side.
	//
	// The retry loop is not cosmetic. Bringing the route up flips the module's
	// f_audio switch, and that can reset the USB gadget's pipes underneath us:
	// the shell dies mid-script, leaving a half-rotated route — a stale mavo
	// still holding hw:0,4, which makes the next session fail with EBUSY
	// (errno 16) and every call after it silent. So a lost reply is reconnected
	// and retried, never trusted, and never treated as "the runtime is missing".
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		err := p.runScript(ctx, res)
		if err == nil {
			return nil
		}
		lastErr = err
		// Believe the live state over a lost reply.
		if ok, herr := p.Healthy(ctx); herr == nil && ok {
			res.ModulesLoaded, res.Calibrated, res.BridgeRunning = true, true, true
			p.logf("provision: route is live (the shell reply was lost when the USB gadget reset)")
			return nil
		}
		// Verify the runtime on every failure, not just on "missing": a module
		// reboot empties /tmp, and the script then reports "modules not loaded"
		// rather than "missing", which used to leave the runtime unpushed and
		// the retry pointless. ensureFiles is md5-checked, so this is cheap when
		// everything is already in place.
		if ferr := p.ensureFiles(ctx, res); ferr != nil {
			return fmt.Errorf("%w (install: %v)", err, ferr)
		}
		p.logf("provision: attempt %d failed: %v", attempt, err)
		if attempt < 3 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Second):
			}
		}
	}
	return lastErr
}

// errRuntimeMissing marks "the module does not have the vendor runtime yet".
var errRuntimeMissing = errors.New("voiceroute: vendor runtime not installed")

// runScript executes the whole provisioning sequence and reads the markers the
// script prints back.
func (p *Provisioner) runScript(ctx context.Context, res *Result) error {
	out, err := p.shell(ctx, p.provisionScript(), provisionTimeout)
	if err != nil {
		return err
	}
	text := normalize(out)
	res.ModulesLoaded = strings.Contains(text, "MODULES=2")
	res.Calibrated = markerCount(text, "CAL-COUNT=") > 0
	res.BridgeRunning = strings.Contains(text, "ROUTE-READY=yes")

	switch {
	case strings.Contains(text, "MAVO-DIR-MISSING"):
		return fmt.Errorf("%w", errRuntimeMissing)
	case !res.ModulesLoaded:
		return fmt.Errorf("voiceroute: kernel modules not loaded (%s)", compact(text))
	case markerCount(text, "MAVO-SURVIVORS=") > 0:
		// A survivor from the previous rotation toggles f_audio/audio_enable
		// back to 0 and every later call is silent (CellBridge, real hardware).
		return fmt.Errorf("voiceroute: an old voice-route session survived the rotation (%s)", compact(text))
	case strings.Contains(text, "FIFO-MISSING"):
		return fmt.Errorf("voiceroute: %s never appeared (%s)", calFIFO, compact(text))
	case !res.Calibrated:
		tail, _ := p.shell(ctx, "tail -n 3 "+calLog+" 2>/dev/null", 10*time.Second)
		return fmt.Errorf("voiceroute: VoLTE calibration did not apply (%s)", strings.TrimSpace(normalize(tail)))
	case !res.BridgeRunning:
		// The route is not streaming until the gadget's f_audio switch is on
		// and both hw:0,4 substreams report RUNNING — a running process alone
		// proves nothing (and is how calls end up silent).
		detail, _ := p.shell(ctx, "tail -n 4 "+routeLog+" 2>/dev/null", 10*time.Second)
		busy := strings.Contains(detail, "errno 16") || strings.Contains(detail, "Busy")
		return fmt.Errorf("voiceroute: voice route is not streaming%s (%s; audio_enable=%s play=%s cap=%s)",
			ternary(busy, " — hw:0,4 is held by a stale session", ""),
			strings.TrimSpace(normalize(detail)),
			markerString(text, "AUDIO-ENABLE="), markerString(text, "PCM-PLAY="), markerString(text, "PCM-CAP="))
	}
	p.logf("provision: ready in %s", time.Since(res.At).Round(time.Millisecond))
	return nil
}

// provisionScript is the module-side sequence, run by one sh.
//
// Kept to plain words and single quotes only: the string travels Go -> ADB ->
// adbd -> busybox ash, and every extra layer of quoting is a chance to get it
// wrong on a device we cannot easily debug.
//
// The route is rotated for every call. CellBridge's hardware notes are blunt
// about why: "the QDC507 UAC gadget's ASYNC capture stream dies after
// aplay/arecord have touched it once. A fresh mavo voice-route session
// restores it" — every silent call they saw followed a reused session. And a
// *surviving* old session is worse than useless: "killall without -9 was
// leaving survivors (3 mavo PIDs alive after rotate, audio_enable toggled to 0
// by one of them, every later call silent)". Hence kill -9 plus a
// zero-survivor check.
func (p *Provisioner) provisionScript() string {
	return fmt.Sprintf(`cd %[1]s 2>/dev/null || { echo MAVO-DIR-MISSING; exit 1; }
# Fail fast when the runtime is not on the module: after a module reboot /tmp
# is empty, and without this the script would spend a minute waiting for a
# calibrator that can never start.
lsmod | grep -q '^qdc507_aprv3 ' || insmod qdc507_aprv3.ko || { echo MAVO-DIR-MISSING; exit 1; }
lsmod | grep -q '^qdc507_voice ' || insmod qdc507_voice.ko || { echo MAVO-DIR-MISSING; exit 1; }
echo MODULES=$(lsmod | grep -c '^qdc507_')

# --- rotate: no survivor may outlive this step -----------------------------
# Match on /proc/PID/exe, never on our own command line: this whole script is
# one shell invocation whose text mentions the helper by name.
attempt=0
while [ $attempt -lt 3 ]; do
  for d in /proc/[0-9]*; do
    pid=${d#/proc/}
    [ "$pid" = "$$" ] && continue
    exe=$(readlink "$d/exe" 2>/dev/null)
    case "$exe" in
      */mavo-pcm-bridge*) grep -qa -- '--voice-route-session' "$d/cmdline" 2>/dev/null && kill -9 "$pid" 2>/dev/null ;;
    esac
  done
  [ -f %[8]s ] && kill -9 $(cat %[8]s) 2>/dev/null
  sleep 0.5
  survivors=0
  for d in /proc/[0-9]*; do
    pid=${d#/proc/}
    [ "$pid" = "$$" ] && continue
    exe=$(readlink "$d/exe" 2>/dev/null)
    case "$exe" in */mavo-pcm-bridge*) survivors=$((survivors+1)) ;; esac
  done
  [ "$survivors" = 0 ] && break
  attempt=$((attempt+1))
done
rm -f %[8]s
echo MAVO-SURVIVORS=$survivors

# --- calibration: reuse the calibrator we already own ----------------------
owned=0
if [ -s %[9]s ]; then
  read cpid cstart < %[9]s
  nowstart=$(cut -d ' ' -f 22 "/proc/$cpid/stat" 2>/dev/null)
  argv0=$(tr '\000' '\n' < "/proc/$cpid/cmdline" 2>/dev/null | sed -n '1p')
  [ "$nowstart" = "$cstart" ] && [ "$argv0" = %[4]s ] && owned=1
fi
if [ "$owned" -eq 0 ]; then
  rm -f %[2]s %[3]s %[9]s
  setsid sh -c 'echo $$ > %[9]s; exec %[4]s </dev/null > %[3]s 2>&1' &
  i=0; while [ ! -p %[2]s ] && [ $i -lt 60 ]; do sleep 0.25; i=$((i+1)); done
fi
[ -p %[2]s ] && echo FIFO-OK || echo FIFO-MISSING
if ! grep -q '%[5]s' %[3]s 2>/dev/null; then
  printf 'open snd_soc_msm_9x07_Tomtom_I2S\n' > %[2]s
  printf 'set _verb VoLTE\n' > %[2]s
  printf 'set _enadev Auxpcm Rx\n' > %[2]s
  printf 'set _enadev Auxpcm Tx\n' > %[2]s
  i=0; while [ $i -lt 100 ]; do grep -q '%[5]s' %[3]s 2>/dev/null && break; sleep 0.25; i=$((i+1)); done
fi
echo CAL-COUNT=$(grep -c '%[5]s' %[3]s 2>/dev/null)

# --- route session ---------------------------------------------------------
setsid sh -c 'echo $$ > %[8]s; exec ./%[6]s --voice-route-session --verbose > %[7]s 2>&1' &
i=0
while [ $i -lt 80 ]; do
  if grep -q '%[10]s' %[7]s 2>/dev/null &&
     [ "$(cat /sys/class/android_usb/f_audio/audio_enable 2>/dev/null)" = 1 ] &&
     grep -q '^state: RUNNING' /proc/asound/card0/pcm4p/sub0/status 2>/dev/null &&
     grep -q '^state: RUNNING' /proc/asound/card0/pcm4c/sub0/status 2>/dev/null; then
    break
  fi
  sleep 0.25; i=$((i+1))
done
if grep -q '%[10]s' %[7]s 2>/dev/null &&
   [ "$(cat /sys/class/android_usb/f_audio/audio_enable 2>/dev/null)" = 1 ] &&
   grep -q '^state: RUNNING' /proc/asound/card0/pcm4p/sub0/status 2>/dev/null &&
   grep -q '^state: RUNNING' /proc/asound/card0/pcm4c/sub0/status 2>/dev/null; then
  echo ROUTE-READY=yes
else
  echo ROUTE-READY=no
fi
echo AUDIO-ENABLE=$(cat /sys/class/android_usb/f_audio/audio_enable 2>/dev/null || echo missing)
echo PCM-PLAY=$(sed -n 's/^state: \(.*\)/\1/p' /proc/asound/card0/pcm4p/sub0/status 2>/dev/null)
echo PCM-CAP=$(sed -n 's/^state: \(.*\)/\1/p' /proc/asound/card0/pcm4c/sub0/status 2>/dev/null)
echo PROVISION-DONE`,
		p.opts.RemoteDir, calFIFO, calLog, calibrator, calRxMarker, bridgeBin, routeLog,
		bridgePidFile, calPidFile, routeReadyMarker)
}

// Script returns the exact module-side command, for debugging.
func (p *Provisioner) Script() string { return p.provisionScript() }

// --- step 1: the vendor runtime --------------------------------------------

func (p *Provisioner) ensureFiles(ctx context.Context, res *Result) error {
	if _, err := p.shell(ctx, "mkdir -p "+p.opts.RemoteDir, 10*time.Second); err != nil {
		return fmt.Errorf("mkdir %s: %w", p.opts.RemoteDir, err)
	}
	remote, err := p.remoteDigests(ctx)
	if err != nil {
		return err
	}
	for i, f := range runtimeFiles {
		src := filepath.Join(p.opts.FilesDir, f.name)
		want, err := digestFile(src)
		if err != nil {
			return fmt.Errorf("vendor runtime: %w", err)
		}
		if remote[f.name] == want {
			p.logf("provision: %s already on the module", f.name)
			continue
		}
		p.logf("provision: pushing %s", f.name)
		if err := p.pushFile(ctx, f, src, want, pushPortBase+i); err != nil {
			return err
		}
		res.Pushed = append(res.Pushed, f.name)
	}
	return nil
}

// remoteDigests asks the module for the md5 of every runtime file in one round
// trip. Missing files are simply absent from the map.
func (p *Provisioner) remoteDigests(ctx context.Context) (map[string]string, error) {
	names := make([]string, 0, len(runtimeFiles))
	for _, f := range runtimeFiles {
		names = append(names, f.name)
	}
	cmd := fmt.Sprintf("cd %s && busybox md5sum %s 2>/dev/null", p.opts.RemoteDir, strings.Join(names, " "))
	out, err := p.shell(ctx, cmd, 20*time.Second)
	if err != nil {
		return nil, err
	}
	digests := map[string]string{}
	for _, line := range strings.Split(normalize(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 {
			digests[fields[1]] = fields[0]
		}
	}
	return digests, nil
}

// pushFile streams one file to a listener the module runs. The module's
// firewall drops everything it initiates towards the Mac, so it cannot pull:
// the module listens with busybox nc and we dial it.
func (p *Provisioner) pushFile(ctx context.Context, f runtimeFile, src, want string, port int) error {
	var lastErr error
	for attempt := 1; attempt <= pushAttempts; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// One round trip: start the listener, give it a moment (the shell must
		// not exit before nc is listening — a listener can be reaped as the
		// session tears down), then prove it is actually bound. -w makes nc
		// give up on its own, so a listener we never reach cannot linger on a
		// device that only has ~43 MB of RAM.
		listen := fmt.Sprintf(
			"cd %s && setsid busybox nc -w 25 -l -p %d > %s 2>/tmp/nc-%d.err & sleep 1; "+
				"netstat -lnt 2>/dev/null | grep -c ':%d '",
			p.opts.RemoteDir, port, f.name, port, port)
		out, err := p.shell(ctx, listen, 25*time.Second)
		if err != nil {
			return fmt.Errorf("start listener for %s: %w", f.name, err)
		}
		if !listening(out) {
			lastErr = fmt.Errorf("%s: listener on :%d never came up", f.name, port)
			p.logf("provision: %v", lastErr)
			continue
		}
		if err := p.sendFile(ctx, port, src); err != nil {
			lastErr = err
			p.logf("provision: %s attempt %d: %v", f.name, attempt, err)
			continue
		}
		got, err := p.remoteDigest(ctx, f.name)
		if err != nil {
			lastErr = err
			continue
		}
		if got != want {
			lastErr = fmt.Errorf("%s: md5 mismatch (want %s, got %s)", f.name, want, got)
			p.logf("provision: %s", lastErr)
			continue
		}
		if f.mode != 0o644 {
			_, _ = p.shell(ctx, fmt.Sprintf("chmod %o %s/%s", f.mode, p.opts.RemoteDir, f.name), 10*time.Second)
		}
		p.logf("provision: %s pushed and verified", f.name)
		return nil
	}
	return fmt.Errorf("push %s: %w", f.name, lastErr)
}

// listening reads the count netstat printed for the listener we just started.
func listening(out string) bool {
	for _, line := range strings.Split(normalize(out), "\n") {
		if n, err := strconv.Atoi(strings.TrimSpace(line)); err == nil {
			return n > 0
		}
	}
	return false
}

func (p *Provisioner) remoteDigest(ctx context.Context, name string) (string, error) {
	cmd := fmt.Sprintf("busybox md5sum %s/%s 2>/dev/null", p.opts.RemoteDir, name)
	out, err := p.shell(ctx, cmd, 20*time.Second)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(normalize(out))
	if len(fields) < 1 {
		return "", fmt.Errorf("%s: module returned no digest", name)
	}
	return fields[0], nil
}

func (p *Provisioner) sendFile(ctx context.Context, port int, src string) error {
	addr := net.JoinHostPort(p.opts.ModuleIP, strconv.Itoa(port))
	deadline := time.Now().Add(10 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		conn, err := p.opts.Dial(ctx, "tcp", addr)
		if err != nil {
			lastErr = err
			time.Sleep(300 * time.Millisecond)
			continue
		}
		defer conn.Close()
		return streamFile(conn, src)
	}
	return fmt.Errorf("module did not accept %s: %w", addr, lastErr)
}

func streamFile(conn net.Conn, src string) error {
	file, err := os.Open(src)
	if err != nil {
		return err
	}
	defer file.Close()

	_ = conn.SetWriteDeadline(time.Now().Add(60 * time.Second))
	if _, err := io.Copy(conn, file); err != nil {
		return fmt.Errorf("send %s: %w", filepath.Base(src), err)
	}
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.CloseWrite()
	}
	// Wait for the module to finish writing: busybox nc closes the socket once
	// its stdin ends, and the file is not complete until then.
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	_, _ = io.Copy(io.Discard, conn)
	return nil
}

// --- diagnostics -----------------------------------------------------------

// Healthy answers "is the route actually streaming right now?" using
// CellBridge's live criteria: a mavo session is alive, the module's f_audio
// switch is on, and both hw:0,4 substreams report RUNNING. A running process
// alone proves nothing — the gadget's capture stream dies as soon as the host
// has touched the UAC pair once, and that is exactly when calls go silent.
//
// It is one shell command, so it is cheap enough to run right after the host
// opens the module's USB audio interface.
func (p *Provisioner) Healthy(ctx context.Context) (bool, error) {
	cmd := "got=0; for d in /proc/[0-9]*; do exe=$(readlink \"$d/exe\" 2>/dev/null); " +
		"case \"$exe\" in */mavo-pcm-bridge*) got=1 ;; esac; done; " +
		"echo MAVO-ALIVE=$got; " +
		"echo AUDIO-ENABLE=$(cat /sys/class/android_usb/f_audio/audio_enable 2>/dev/null || echo missing); " +
		"echo PCM-PLAY=$(sed -n 's/^state: \\(.*\\)/\\1/p' /proc/asound/card0/pcm4p/sub0/status 2>/dev/null); " +
		"echo PCM-CAP=$(sed -n 's/^state: \\(.*\\)/\\1/p' /proc/asound/card0/pcm4c/sub0/status 2>/dev/null)"
	out, err := p.shell(ctx, cmd, 20*time.Second)
	if err != nil {
		return false, err
	}
	text := normalize(out)
	if markerCount(text, "MAVO-ALIVE=") == 0 {
		return false, nil
	}
	switch markerString(text, "AUDIO-ENABLE=") {
	case "1":
	default:
		// "missing" on a firmware without the knob is not a fault; 0 is.
		if markerString(text, "AUDIO-ENABLE=") == "0" {
			return false, nil
		}
	}
	return markerString(text, "PCM-PLAY=") == "RUNNING" && markerString(text, "PCM-CAP=") == "RUNNING", nil
}

// Check reports what the module currently thinks. It is what the HTTP API
// exposes so a failed call can be diagnosed without a terminal on the module.
func (p *Provisioner) Check(ctx context.Context) (map[string]string, error) {
	if p.opts.ADB == nil {
		return nil, errors.New("voiceroute: no adb client")
	}
	cmd := fmt.Sprintf(
		"echo FILES; ls -l %s 2>&1; echo MODULES; lsmod | grep -E '^qdc507' ; "+
			"echo SND; ls /dev/snd 2>&1 | tr '\\n' ' '; echo; echo PROCS; ps | grep -E '[m]avo|[a]lsaucm' ; "+
			"echo CAL; grep -o 'Mixer VoLTE' %s 2>/dev/null | head -2; echo CALLINE; grep -c 'SEC_AUX_PCM_RX_Voice Mixer VoLTE' %s 2>/dev/null; "+
			"echo USB-AUDIO; cat /sys/class/android_usb/f_audio/audio_enable 2>&1; echo VOICE-BRIDGE; ls -l /dev/ttyGS0 /run/voc_svr 2>&1; "+
			"echo PCM4; for f in /proc/asound/card0/pcm4p/sub0/status /proc/asound/card0/pcm4c/sub0/status; do "+
			"printf '%%s ' \"$f\"; sed -n 's/^state: //p' \"$f\" 2>/dev/null || echo '(missing)'; done; "+
			"echo ROUTELOG; tail -n 3 %s 2>&1",
		p.opts.RemoteDir, calLog, calLog, routeLog)
	out, err := p.shell(ctx, cmd, 40*time.Second)
	if err != nil {
		return nil, err
	}
	return map[string]string{"raw": normalize(out)}, nil
}

// --- plumbing --------------------------------------------------------------

func (p *Provisioner) shell(ctx context.Context, cmd string, timeout time.Duration) (string, error) {
	out, err := p.opts.ADB.Shell(ctx, cmd, timeout)
	if err != nil {
		return out, err
	}
	return out, nil
}

func (p *Provisioner) logf(format string, args ...any) { p.opts.Logf(format, args...) }

func normalize(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }

func ternary(cond bool, yes, no string) string {
	if cond {
		return yes
	}
	return no
}

// markerCount reads "NAME=<n>" out of the script's output.
func markerCount(out, marker string) int {
	if v := markerString(out, marker); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return 0
}

// markerString reads the value of "NAME=value" out of the script's output.
func markerString(out, marker string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, marker) {
			continue
		}
		return strings.TrimSpace(strings.TrimPrefix(line, marker))
	}
	return ""
}

// compact flattens module output into something short enough for a log line.
func compact(s string) string {
	fields := strings.Fields(s)
	if len(fields) > 12 {
		fields = fields[:12]
	}
	return strings.Join(fields, " ")
}

func digestFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
