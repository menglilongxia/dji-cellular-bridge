package gateway

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync/atomic"
	"time"

	"github.com/dji-cellular-bridge/mac-gateway/internal/adb"
	"github.com/dji-cellular-bridge/mac-gateway/internal/voice"
	"github.com/dji-cellular-bridge/mac-gateway/internal/voiceroute"
)

// AudioDevices lists the CoreAudio devices this process can see (name and
// direction), which is how the module's UAC pair is picked.
func (c *Controller) AudioDevices() (string, error) {
	return voice.Devices("")
}

// AudioProbe opens one input device through the gateway's own code path for a
// few seconds and reports frames and peak amplitude.
//
// This is the decisive microphone-permission test: macOS does not fail a
// denied capture, it hands back silence, so a silent call has to be told apart
// from a denied one. Speak into the chosen device while this runs — a non-zero
// peak means the gateway itself is allowed to capture.
func (c *Controller) AudioProbe(ctx context.Context, device string, seconds int) (map[string]any, error) {
	if device == "" {
		device = c.cfg.AudioDevice
	}
	if seconds <= 0 || seconds > 30 {
		seconds = 5
	}
	var frames, peak, chunks atomic.Int64
	dev, err := voice.OpenCapture(device, func(pcm []int16) {
		chunks.Add(1)
		frames.Add(int64(len(pcm)))
		for _, s := range pcm {
			v := int64(s)
			if v < 0 {
				v = -v
			}
			for {
				old := peak.Load()
				if v <= old || peak.CompareAndSwap(old, v) {
					break
				}
			}
		}
	})
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(time.Duration(seconds) * time.Second)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		time.Sleep(200 * time.Millisecond)
	}
	dev.Close()
	return map[string]any{
		"device":  device,
		"seconds": seconds,
		"frames":  frames.Load(),
		"chunks":  chunks.Load(),
		"peak":    peak.Load(),
		"verdict": audioProbeVerdict(frames.Load(), peak.Load()),
	}, nil
}

func audioProbeVerdict(frames, peak int64) string {
	switch {
	case frames == 0:
		return "no frames at all — the device never delivered audio"
	case peak == 0:
		return "frames arrived but every sample is zero: microphone permission is denied (or the source is silent)"
	default:
		return "capture is working (non-zero audio reached this process)"
	}
}

// ATCommand runs one raw AT command through the modem (diagnostics only).
// DJOneHub logs AT+QCFG="USBCFG"? / AT+QPCMV? / AT+QDAI? once at startup for
// exactly this reason: when a call is silent, the module's own USB-audio and
// IMS configuration is the first thing to look at.
func (c *Controller) ATCommand(ctx context.Context, cmd string) (string, error) {
	runner, ok := c.modem.(interface {
		ATTimeout(ctx context.Context, cmd string, timeout time.Duration) (string, error)
	})
	if !ok {
		return "", errors.New("voice: this modem driver does not expose raw AT")
	}
	return runner.ATTimeout(ctx, cmd, 15*time.Second)
}

// VoiceDiag dumps the module-side VoIP configuration the same way DJOneHub
// does when a call carries no audio.
func (c *Controller) VoiceDiag(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}
	for _, cmd := range []string{
		`AT+QCFG="ims"`,
		`AT+QCFG="volte/disable"`,
		`AT+QCFG="USBCFG"`,
		"AT+QPCMV?",
		"AT+QDAI?",
		"AT+CLCC",
	} {
		res, err := c.ATCommand(ctx, cmd)
		if err != nil {
			out[cmd] = "error: " + err.Error()
			continue
		}
		out[cmd] = strings.TrimSpace(res)
	}
	return out, nil
}

// voiceProvisioner returns the provisioner, building it the first time the
// module's ADB interface is actually available.
func (c *Controller) voiceProvisioner() *voiceroute.Provisioner {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.voice != nil || c.adbProvider == nil {
		return c.voice
	}
	client := c.adbProvider.ADBClient()
	if client == nil {
		c.voiceErr = errors.New("voice: module exposes no ADB interface (check the usbcfg composition)")
		return nil
	}
	c.voice = voiceroute.New(voiceroute.Options{
		ADB:       client,
		FilesDir:  c.cfg.VoiceFilesDir,
		ModuleIP:  c.cfg.ModuleIP,
		RemoteDir: voiceroute.DefaultRemoteDir,
		Logf:      func(format string, args ...any) { log.Printf("voice: "+format, args...) },
	})
	c.voiceErr = nil
	return c.voice
}

// voiceWatchdog re-checks the route shortly after the host opened the module's
// USB audio device. CellBridge documents a UAC firmware quirk where opening a
// playback stream can take the route down; if that happened the call would be
// silent, so put it back up.
func (c *Controller) voiceWatchdog() {
	p := c.voiceProvisioner()
	if p == nil {
		return
	}
	go func() {
		// Host-side opening of the UAC pair resets the module's USB gadget
		// pipes, and that aborts our ADB traffic for a moment too. So a failed
		// probe means "ask again", never "the route died" — rebuilding on a
		// hiccup would cost the first seconds of an otherwise fine call.
		var lastErr error
		for attempt := 1; attempt <= 4; attempt++ {
			time.Sleep(time.Duration(attempt) * 1200 * time.Millisecond)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			ok, err := p.Healthy(ctx)
			cancel()
			if err == nil && ok {
				return
			}
			lastErr = err
		}
		// Only re-provision (without USB reset) during a call. The full
		// provisionVoice with USB recovery is too destructive mid-call.
		log.Printf("voice: route not streaming during call (%v) — attempting soft re-provision", lastErr)
		softCtx, softCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer softCancel()
		if p2 := c.voiceProvisioner(); p2 != nil {
			if res, err := p2.Start(softCtx); err != nil {
				log.Printf("voice: soft re-provision failed: %v (call audio may be degraded)", err)
			} else {
				log.Printf("voice: soft re-provision ok in %s", res.Duration.Round(time.Millisecond))
			}
		}
	}()
}

// voiceStart provisions the module-side VoLTE route.
//
// It is called as soon as a call is signalled in either direction — an
// incoming RING or an outgoing dial — never when the call is answered:
// provisioning takes seconds (kernel modules, ACDB calibration, PCM bridge),
// and waiting for the answer guarantees the first seconds of the call are
// silent. Measured on the real module: 2.7 s with the runtime already
// installed, ~7 s when the 1 MB runtime has to be pushed first.
//
// Everything runs in the background; a failure is logged, not fatal. The call
// still connects, it just carries no audio.
func (c *Controller) voiceStart() {
	p := c.voiceProvisioner()
	if p == nil {
		c.runVoiceScript("start")
		return
	}
	go c.provisionVoice()
}

// voiceEnsure is the call-answered hook. The route was already provisioned
// when the call started ringing, and re-running the sequence now would tear a
// working route down for ~4 s right as the callee picks up. It only steps in
// when there is nothing running yet — a provisioning that failed, or one that
// never happened.
func (c *Controller) voiceEnsure() {
	p := c.voiceProvisioner()
	if p == nil {
		return // the script path already ran at ring time
	}
	last := p.Last()
	// A route rotated while the phone was still ringing is fine to reuse, but
	// only for a while: CellBridge treats a rotation older than 30 s as stale
	// and re-rotates on answer, and a long ring is exactly how a route goes
	// stale before the call is picked up.
	if !last.At.IsZero() && last.Err == nil && time.Since(last.At) < 30*time.Second {
		return
	}
	go c.provisionVoice()
}

func (c *Controller) voiceStop() {
	p := c.voiceProvisioner()
	if p == nil {
		c.runVoiceScript("stop")
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := p.Stop(ctx); err != nil {
			log.Printf("voice: stop failed: %v", err)
		}
	}()
}

// provisionVoice runs the provisioning sequence, and if the module's adbd has
// wedged — it answers the handshake but closes every service open — resets the
// USB device (we are the only process that can) and tries once more.
func (c *Controller) provisionVoice() {
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()

	p := c.voiceProvisioner()
	if p == nil {
		log.Printf("voice: %v", c.voiceErr)
		return
	}
	res, err := p.Start(ctx)
	if err == nil {
		log.Printf("voice: route ready in %s%s", res.Duration.Round(time.Millisecond), pushedSuffix(res.Pushed))
		return
	}
	log.Printf("voice: provisioning failed after %s: %v", res.Duration.Round(time.Millisecond), err)

	if c.adbRecover == nil || !errors.Is(err, adb.ErrServiceClosed) {
		return
	}
	// Never reboot the module during an active call: a USB reset kills the
	// call, causes CoreAudio to renegotiate at 48 kHz, and makes both
	// directions go silent. Defer recovery until after the call ends.
	if c.audio.running() {
		log.Printf("voice: adbd is wedged but a call is active — deferring USB recovery until call ends")
		return
	}
	log.Printf("voice: module adbd is wedged; resetting the USB device and retrying once")
	rctx, rcancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer rcancel()
	if rerr := c.adbRecover.RecoverADB(rctx); rerr != nil {
		log.Printf("voice: usb recovery failed: %v", rerr)
		return
	}
	res2, err2 := p.Start(rctx)
	if err2 != nil {
		log.Printf("voice: provisioning still failing after recovery: %v", err2)
		return
	}
	log.Printf("voice: route ready after usb recovery in %s%s", res2.Duration.Round(time.Millisecond), pushedSuffix(res2.Pushed))
}

func pushedSuffix(pushed []string) string {
	if len(pushed) == 0 {
		return ""
	}
	return fmt.Sprintf(" (pushed %v)", pushed)
}

// VoiceEnabled reports whether the in-process ADB provisioner is in use.
func (c *Controller) VoiceEnabled() bool { return c.voiceProvisioner() != nil }

// VoiceStatus is the payload of GET /api/v1/voice.
func (c *Controller) VoiceStatus() map[string]any {
	p := c.voiceProvisioner()
	if p == nil {
		return map[string]any{
			"enabled": false,
			"script":  c.cfg.VoiceScript,
			"error":   errString(c.voiceErr),
		}
	}
	last := p.Last()
	out := map[string]any{
		"enabled": true,
		"running": p.Running(),
	}
	if !last.At.IsZero() {
		out["last"] = map[string]any{
			"at":             last.At.Format(time.RFC3339),
			"duration_ms":    last.Duration.Milliseconds(),
			"pushed":         last.Pushed,
			"modules_loaded": last.ModulesLoaded,
			"calibrated":     last.Calibrated,
			"bridge_running": last.BridgeRunning,
			"error":          errString(last.Err),
		}
	}
	return out
}

// VoiceStart triggers provisioning by hand (same code path a call takes).
func (c *Controller) VoiceStart(ctx context.Context) (map[string]any, error) {
	p := c.voiceProvisioner()
	if p == nil {
		return nil, errors.New("voice: in-process provisioning is not enabled")
	}
	res, err := p.Start(ctx)
	out := map[string]any{
		"duration_ms":    res.Duration.Milliseconds(),
		"pushed":         res.Pushed,
		"modules_loaded": res.ModulesLoaded,
		"calibrated":     res.Calibrated,
		"bridge_running": res.BridgeRunning,
	}
	if err != nil {
		out["error"] = err.Error()
		return out, err
	}
	return out, nil
}

// VoiceStop tears the voice route down.
func (c *Controller) VoiceStop(ctx context.Context) error {
	p := c.voiceProvisioner()
	if p == nil {
		return errors.New("voice: in-process provisioning is not enabled")
	}
	return p.Stop(ctx)
}

// VoiceCheck asks the module what it currently thinks (files, kernel modules,
// ALSA devices, running processes, calibration markers).
func (c *Controller) VoiceCheck(ctx context.Context) (map[string]string, error) {
	p := c.voiceProvisioner()
	if p == nil {
		return nil, errors.New("voice: in-process provisioning is not enabled")
	}
	return p.Check(ctx)
}

// VoiceRecover resets the USB device to revive a wedged adbd.
func (c *Controller) VoiceRecover(ctx context.Context) error {
	if c.adbRecover == nil {
		return errors.New("voice: this modem driver cannot recover the USB device")
	}
	return c.adbRecover.RecoverADB(ctx)
}

// ModuleShell runs one read-only diagnostic command on the module over the
// gateway's ADB channel. It exists so a silent call can be diagnosed without
// stopping the gateway and fighting it for the USB device.
func (c *Controller) ModuleShell(ctx context.Context, cmd string) (string, error) {
	client := c.adbProvider
	if client == nil {
		return "", errors.New("voice: no ADB channel on this modem driver")
	}
	adb := client.ADBClient()
	if adb == nil {
		return "", errors.New("voice: module exposes no ADB interface")
	}
	return adb.Shell(ctx, cmd, 25*time.Second)
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
