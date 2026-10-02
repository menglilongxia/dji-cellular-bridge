package gateway

import (
	"log"
	"math"
	"net"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dji-cellular-bridge/mac-gateway/internal/rtp"
	"github.com/dji-cellular-bridge/mac-gateway/internal/sip"
	"github.com/dji-cellular-bridge/mac-gateway/internal/voice"
)

// farFilter: 4th-order Butterworth low-pass at 3.0 kHz for module->phone.
// The module's USB audio has strong out-of-band noise at 3.5-4 kHz.
type farFilter struct {
	b0, b1, b2, a1, a2 float64
	z1                 [2]float64
	z2                 [2]float64
	on                 bool
}

func newFarFilter(cutoffHz, sampleRate float64) *farFilter {
	w0 := 2 * math.Pi * cutoffHz / sampleRate
	cw, sw := math.Cos(w0), math.Sin(w0)
	alpha := sw * 0.7071067811865476
	b0 := (1 - cw) / 2
	b1 := 1 - cw
	b2 := (1 - cw) / 2
	a0 := 1 + alpha
	a1 := -2 * cw
	a2 := 1 - alpha
	return &farFilter{
		b0: b0 / a0, b1: b1 / a0, b2: b2 / a0, a1: a1 / a0, a2: a2 / a0,
		on: true,
	}
}

func (f *farFilter) process(pcm []int16) {
	if f == nil || !f.on {
		return
	}
	for i, s := range pcm {
		x := float64(s)
		for stage := 0; stage < 2; stage++ {
			o := f.b0*x + f.z1[stage]
			f.z1[stage] = f.b1*x - f.a1*o + f.z2[stage]
			f.z2[stage] = f.b2*x - f.a2*o
			x = o
		}
		v := int32(x + math.Copysign(0.5, x))
		switch {
		case v > 32767:
			v = 32767
		case v < -32768:
			v = -32768
		}
		pcm[i] = int16(v)
	}
}

// runVoiceScript invokes tools/module-voice.sh in the background.
func (c *Controller) runVoiceScript(action string) {
	script := c.cfg.VoiceScript
	if script == "" {
		return
	}
	voiceMu.Lock()
	go func() {
		defer voiceMu.Unlock()
		out, err := exec.Command(script, action).CombinedOutput()
		if err != nil {
			log.Printf("voice: %s %s failed: %v (%s)", script, action, err,
				strings.TrimSpace(string(out)))
			return
		}
		log.Printf("voice: %s -> %s", action, strings.TrimSpace(string(out)))
	}()
}

var voiceMu sync.Mutex

// audioBridge connects the phone's RTP stream to the module's USB Audio.
//
// Key design: the RTP socket on port 40000 is created once and persists across
// calls. The iOS app also keeps one NWConnection alive across calls (to work
// around the iOS background local-network restriction). If the gateway creates
// a new socket per call, the phone's existing connection doesn't follow, and
// both directions go silent.
//
// Only the CoreAudio device is opened/closed per call.
type audioBridge struct {
	deviceName string
	enabled    bool
	rtpPort    int

	mu   sync.Mutex
	dev  *voice.Device
	rtp  *rtp.Session // persistent, survives stop()
	peer *net.UDPAddr
	call string // current call ID, "" when idle

	// Per-call counters.
	inFrames  atomic.Int64
	inPeak    atomic.Int64
	outFrames atomic.Int64
	rtpIn     atomic.Int64
	rtpOut    atomic.Int64
	far       *farFilter

	// Sample rate monitoring: detect CoreAudio rate drift after USB reset.
	rateCheckFrames atomic.Int64
	rateCheckAt     atomic.Int64 // unix nano
}

const (
	defaultRTPPort  = 40000
	expectedRate    = 8000.0
	rateTolerance   = 0.15 // 15% drift triggers device reopen
	rateCheckWindow = 3    // seconds between checks
)

func newAudioBridge(enabled bool, deviceName string) *audioBridge {
	ab := &audioBridge{
		enabled:    enabled,
		deviceName: deviceName,
		rtpPort:    defaultRTPPort,
		far:        newFarFilter(3000, 8000),
	}
	if enabled {
		ab.ensureRTP()
	}
	return ab
}

// ensureRTP creates the persistent RTP socket if it doesn't exist.
func (a *audioBridge) ensureRTP() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.rtp != nil {
		return
	}
	rs, err := rtp.Listen(a.rtpPort, false)
	if err != nil {
		log.Printf("audio: rtp listen :%d: %v", a.rtpPort, err)
		return
	}
	a.rtp = rs
	go rs.Run()
	log.Printf("audio: persistent RTP socket on :%d", a.rtpPort)
}

// RTPPort returns the port the persistent socket is on.
func (a *audioBridge) RTPPort() int {
	if a == nil {
		return defaultRTPPort
	}
	return a.rtpPort
}

// start wires the given SIP session's RTP to the module audio.
func (a *audioBridge) start(sess *sip.Session) {
	if a == nil || !a.enabled {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.call != "" {
		return // already bridging
	}

	// Reset per-call counters.
	a.inFrames.Store(0)
	a.inPeak.Store(0)
	a.outFrames.Store(0)
	a.rtpIn.Store(0)
	a.rtpOut.Store(0)
	a.rateCheckFrames.Store(0)
	a.rateCheckAt.Store(time.Now().UnixNano())
	a.far = newFarFilter(3000, 8000)

	peer := sess.RemoteRTP()
	if peer == nil {
		log.Printf("audio: no remote RTP for call %s", sess.ID)
		return
	}

	// Make sure persistent RTP socket exists.
	if a.rtp == nil {
		rs, err := rtp.Listen(a.rtpPort, false)
		if err != nil {
			log.Printf("audio: rtp listen :%d: %v", a.rtpPort, err)
			return
		}
		a.rtp = rs
		go rs.Run()
	}

	// Wire phone->module on the persistent socket.
	// Symmetric RTP (latching): when the phone is behind NAT (4G, etc.) the
	// SDP address is wrong. Use the actual source of the first received RTP
	// packet as the real peer — this is what every production SBC does.
	a.rtp.OnPacket = func(payload []byte, src *net.UDPAddr) {
		a.rtpIn.Add(1)
		// Latch: adopt the real sender if it differs from the SDP address.
		if src != nil {
			a.mu.Lock()
			if a.peer != nil && src.String() != a.peer.String() {
				log.Printf("audio: NAT latch — updating peer from %s to %s", a.peer, src)
				a.peer = src
			}
			a.mu.Unlock()
		}
		pcm := rtp.DecodeFrame(payload)
		a.outFrames.Add(int64(len(pcm)))
		a.mu.Lock()
		d := a.dev
		a.mu.Unlock()
		if d != nil {
			_ = d.Play(pcm)
		}
	}

	dev, err := voice.Open(a.deviceName, func(pcm []int16) {
		// Module mic -> RTP (20 ms frames).
		// Check sample rate: if the device is delivering at wrong rate,
		// drop frames instead of sending garbage.
		if a.checkRateDrift(len(pcm)) {
			return // rate is bad, frames dropped
		}
		a.far.process(pcm)
		a.inFrames.Add(int64(len(pcm)))
		for _, s := range pcm {
			v := int64(s)
			if v < 0 {
				v = -v
			}
			for {
				old := a.inPeak.Load()
				if v <= old || a.inPeak.CompareAndSwap(old, v) {
					break
				}
			}
		}
		a.mu.Lock()
		rs := a.rtp
		a.mu.Unlock()
		if rs == nil {
			return
		}
		for off := 0; off+160 <= len(pcm); off += 160 {
			if err := rs.Send(rtp.EncodeFrame(pcm[off:off+160]), peer); err != nil {
				return
			}
			a.rtpOut.Add(1)
		}
	})
	if err != nil {
		log.Printf("audio: %v (falling back to silence)", err)
		return
	}

	a.dev = dev
	a.peer = peer
	a.call = sess.ID
	log.Printf("audio: bridging call %s via \"%s\" (rtp :%d <-> module)", sess.ID, a.deviceName, a.rtpPort)
	go a.reportStats(sess.ID)
}

// checkRateDrift monitors the actual sample rate. Returns true if rate is bad
// and the caller should drop the frame. If drift persists, schedules a device
// reopen on a separate goroutine.
func (a *audioBridge) checkRateDrift(samples int) bool {
	a.rateCheckFrames.Add(int64(samples))
	startNano := a.rateCheckAt.Load()
	elapsed := time.Since(time.Unix(0, startNano)).Seconds()
	if elapsed < rateCheckWindow {
		return false
	}
	frames := a.rateCheckFrames.Swap(0)
	a.rateCheckAt.Store(time.Now().UnixNano())
	rate := float64(frames) / elapsed
	drift := math.Abs(rate-expectedRate) / expectedRate
	if drift <= rateTolerance {
		return false
	}
	// Rate has drifted beyond tolerance. Log and trigger device reopen.
	log.Printf("audio: sample rate drift detected: %.0f Hz (expected %.0f, drift %.0f%%) — reopening device",
		rate, expectedRate, drift*100)
	go a.reopenDevice()
	return true
}

// reopenDevice closes and reopens the CoreAudio device. Called when sample
// rate drift is detected (typically after a USB reset).
func (a *audioBridge) reopenDevice() {
	a.mu.Lock()
	oldDev := a.dev
	callID := a.call
	peer := a.peer
	a.dev = nil
	a.mu.Unlock()

	if oldDev != nil {
		oldDev.Close()
	}
	if callID == "" || peer == nil {
		return
	}
	time.Sleep(200 * time.Millisecond) // let CoreAudio settle

	// Reset rate check state.
	a.rateCheckFrames.Store(0)
	a.rateCheckAt.Store(time.Now().UnixNano())

	dev, err := voice.Open(a.deviceName, func(pcm []int16) {
		if a.checkRateDrift(len(pcm)) {
			return
		}
		a.far.process(pcm)
		a.inFrames.Add(int64(len(pcm)))
		for _, s := range pcm {
			v := int64(s)
			if v < 0 {
				v = -v
			}
			for {
				old := a.inPeak.Load()
				if v <= old || a.inPeak.CompareAndSwap(old, v) {
					break
				}
			}
		}
		a.mu.Lock()
		rs := a.rtp
		a.mu.Unlock()
		if rs == nil {
			return
		}
		for off := 0; off+160 <= len(pcm); off += 160 {
			if err := rs.Send(rtp.EncodeFrame(pcm[off:off+160]), peer); err != nil {
				return
			}
			a.rtpOut.Add(1)
		}
	})
	if err != nil {
		log.Printf("audio: reopen failed: %v — call %s has no module audio", err, callID)
		return
	}
	a.mu.Lock()
	a.dev = dev
	a.mu.Unlock()
	log.Printf("audio: device reopened for call %s", callID)
}

// reportStats logs health lines every 3 seconds during a call.
func (a *audioBridge) reportStats(callID string) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	var (
		lastFrames int64
		lastAt     = time.Now()
	)
	for range ticker.C {
		a.mu.Lock()
		active := a.call == callID
		a.mu.Unlock()
		if !active {
			return
		}
		raw, from := int64(0), ""
		a.mu.Lock()
		rs := a.rtp
		a.mu.Unlock()
		if rs != nil {
			raw, from = rs.RawReceived()
		}
		frames := a.inFrames.Load()
		now := time.Now()
		rate := float64(frames-lastFrames) / now.Sub(lastAt).Seconds()
		lastFrames, lastAt = frames, now
		log.Printf("audio: %s mic_frames=%d mic_rate=%.0fHz mic_peak=%d played_frames=%d rtp_in=%d rtp_out=%d raw_in=%d last_from=%s peer=%s",
			callID, frames, rate, a.inPeak.Load(), a.outFrames.Load(), a.rtpIn.Load(), a.rtpOut.Load(), raw, from, a.peerString())
	}
}

func (a *audioBridge) peerString() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.peer == nil {
		return "-"
	}
	return a.peer.String()
}

func (a *audioBridge) running() bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.call != ""
}

func (a *audioBridge) currentRTP() *rtp.Session {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.rtp
}

// stop tears down the CoreAudio device but keeps the RTP socket alive.
func (a *audioBridge) stop() {
	if a == nil {
		return
	}
	a.mu.Lock()
	dev := a.dev
	a.dev = nil
	a.peer = nil
	a.call = ""
	// Disconnect the packet handler so stale packets don't try to play
	// into a closed device.
	if a.rtp != nil {
		a.rtp.OnPacket = nil
	}
	a.mu.Unlock()

	if dev != nil {
		dev.Close()
	}
	time.Sleep(50 * time.Millisecond)
}

// Close shuts everything down (called on gateway shutdown).
func (a *audioBridge) Close() {
	if a == nil {
		return
	}
	a.stop()
	a.mu.Lock()
	rs := a.rtp
	a.rtp = nil
	a.mu.Unlock()
	if rs != nil {
		rs.Close()
	}
}
