//go:build darwin && cgo

// USBATModem drives a real DJI QDC507 module over the user-space USB AT
// transport (see docs/HARDWARE-SPIKE.md). The module has no /dev/tty.* node on
// macOS, so this is the only way to reach it.
package modem

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"encoding/hex"
	"unicode/utf16"

	"github.com/dji-cellular-bridge/mac-gateway/internal/adb"
)

// USBATModem implements Driver on top of the USB AT transport.
type USBATModem struct {
	// usbMu guards the USB handle. It is swapped out during a device reset
	// (see RecoverADB), and the reader goroutines must never touch a handle
	// that is being torn down. Readers take the read lock for the duration of
	// one transfer (≤1 s), so recovery waits at most that long.
	usbMu sync.RWMutex
	usb   *USBAT

	// adbMu guards the lazily created ADB client bound to this module.
	adbMu     sync.Mutex
	adbClient *adb.Client

	recoverMu sync.Mutex

	// usbOK is the unix-nano time of the last successful USB transfer. The
	// module re-enumerates whenever it reboots (AT+CFUN=1,1, a crash, a power
	// glitch), which invalidates every handle we hold; the watchdog uses this
	// timestamp to notice that and re-attach on its own.
	usbOK atomic.Int64
	// usbRetry is the last time the watchdog tried to re-attach.
	usbRetry atomic.Int64

	atLines chan string // response lines from the AT interface
	urcs    chan Event

	cmdMu sync.Mutex // serialises AT transactions

	stateMu     sync.Mutex
	ringing     bool // a RING is in progress (the module repeats RING every few seconds)
	dialing     bool // an outgoing call was placed and has not ended yet
	pendingCLIP string
	ringTimer   *time.Timer

	callPollMu  sync.Mutex
	callPolling bool

	reattachFails int

	done      chan struct{}
	closeOnce sync.Once
}

var _ Driver = (*USBATModem)(nil)

// NewUSBATModem opens the module and initialises it for voice + SMS.
func NewUSBATModem() (*USBATModem, error) {
	// The module enumerates under a different vid depending on its usbcfg
	// composition, so try both known ids.
	var (
		usb     *USBAT
		lastErr error
	)
	for _, vid := range QDC507VendorIDs {
		usb, lastErr = OpenUSBAT(vid)
		if lastErr == nil {
			break
		}
	}
	if usb == nil {
		return nil, lastErr
	}
	m := &USBATModem{
		usb:     usb,
		atLines: make(chan string, 64),
		urcs:    make(chan Event, 32),
		done:    make(chan struct{}),
	}
	// The module keeps a URC backlog (RING / NO CARRIER from before we opened
	// it). Discard it, otherwise the gateway "rings" on startup for calls that
	// are long over.
	m.drainURC(700 * time.Millisecond)
	go m.readAT()
	go m.readURC()
	go m.watchUSB()
	return m, nil
}

// watchUSB re-attaches to the module after it has re-enumerated.
//
// Without it a module reboot leaves the gateway holding dead handles forever:
// every AT command fails, calls cannot be answered, and only a gateway restart
// fixes it. The readers poll continuously, so "no successful transfer for a
// few seconds" is a reliable signal that the device is gone.
func (m *USBATModem) watchUSB() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.done:
			return
		case <-ticker.C:
		}
		last := m.usbOK.Load()
		if last == 0 || time.Since(time.Unix(0, last)) < 6*time.Second {
			continue
		}
		if since := time.Since(time.Unix(0, m.usbRetry.Load())); since < 5*time.Second {
			continue
		}
		m.usbRetry.Store(time.Now().UnixNano())
		if err := m.reattach(context.Background()); err != nil {
			m.reattachFails++
			// Log first failure and then only every 10th to avoid spamming.
			if m.reattachFails == 1 || m.reattachFails%10 == 0 {
				log.Printf("qdc507: re-attach failed (attempt %d, last %s ago): %v",
					m.reattachFails, time.Since(time.Unix(0, last)).Round(time.Second), err)
			}
			continue
		}
		if m.reattachFails > 0 {
			log.Printf("qdc507: re-attached after %d failed attempts", m.reattachFails)
		} else {
			log.Printf("qdc507: re-attached to the module")
		}
		m.reattachFails = 0
	}
}

// reattach drops the stale handle and opens the module again.
func (m *USBATModem) reattach(ctx context.Context) error {
	m.detachUSB()
	u, err := openAnyModule()
	if err != nil {
		return err
	}
	m.usbMu.Lock()
	m.usb = u
	m.usbMu.Unlock()
	// A fresh handle: the ADB interface must be claimed again, and the ADB
	// client has to forget the session that died with the old device.
	if err := u.OpenADB(); err != nil {
		log.Printf("qdc507: adb unavailable after re-attach: %v", err)
	}
	m.adbMu.Lock()
	client := m.adbClient
	m.adbMu.Unlock()
	if client != nil {
		client.Reset()
	}
	m.usbOK.Store(time.Now().UnixNano())
	m.drainURC(300 * time.Millisecond)
	if err := m.EnableCallerID(ctx); err != nil {
		log.Printf("qdc507: re-init after re-attach: %v", err)
	}
	return nil
}

// currentUSB returns the live USB transport, or nil while the device is being
// reset.
func (m *USBATModem) currentUSB() *USBAT {
	m.usbMu.RLock()
	defer m.usbMu.RUnlock()
	return m.usb
}

var errNoUSBTransport = errors.New("qdc507: usb transport is not open")

// writeAT sends raw bytes to the AT interface.
func (m *USBATModem) writeAT(p []byte) error {
	m.usbMu.RLock()
	defer m.usbMu.RUnlock()
	if m.usb == nil {
		return errNoUSBTransport
	}
	err := m.usb.Write(p)
	m.noteUSB(err)
	return err
}

// readAT reads from the AT interface.
func (m *USBATModem) readATChunk(p []byte, timeoutMs int) (int, error) {
	m.usbMu.RLock()
	defer m.usbMu.RUnlock()
	if m.usb == nil {
		return 0, errNoUSBTransport
	}
	n, err := m.usb.ReadAT(p, timeoutMs)
	m.noteUSB(err)
	return n, err
}

// readURCChunk reads from the URC interface.
func (m *USBATModem) readURCChunk(p []byte, timeoutMs int) (int, error) {
	m.usbMu.RLock()
	defer m.usbMu.RUnlock()
	if m.usb == nil {
		return 0, errNoUSBTransport
	}
	n, err := m.usb.ReadURC(p, timeoutMs)
	m.noteUSB(err)
	return n, err
}

// noteUSB records whether the last transfer worked; the watchdog re-attaches
// once the device has been silent for a few seconds.
func (m *USBATModem) noteUSB(err error) {
	if err == nil {
		m.usbOK.Store(time.Now().UnixNano())
	}
}

// drainURC reads and drops whatever the URC interface has buffered, until the
// line has been quiet for quietFor.
func (m *USBATModem) drainURC(quietFor time.Duration) {
	buf := make([]byte, 4096)
	deadline := time.Now().Add(quietFor)
	for time.Now().Before(deadline) {
		n, err := m.readURCChunk(buf, 150)
		if err != nil {
			break
		}
		if n > 0 {
			deadline = time.Now().Add(quietFor)
		}
	}
}

func (m *USBATModem) Name() string { return "qdc507-usb" }

// AT sends one raw AT command and returns the response text (without the
// trailing OK). Exposed for tooling and diagnostics.
func (m *USBATModem) AT(ctx context.Context, cmd string) (string, error) {
	return m.command(ctx, 8*time.Second, cmd)
}

// ATTimeout is AT with an explicit timeout (AT+CLAC for instance returns a very
// long list).
func (m *USBATModem) ATTimeout(ctx context.Context, cmd string, timeout time.Duration) (string, error) {
	return m.command(ctx, timeout, cmd)
}

func (m *USBATModem) Close() error {
	m.closeOnce.Do(func() {
		close(m.done)
		m.usbMu.Lock()
		u := m.usb
		m.usb = nil
		m.usbMu.Unlock()
		if u != nil {
			_ = u.Close()
		}
	})
	return nil
}

// --- ADB control channel ---------------------------------------------------

// ADBClient returns the ADB client bound to this module's ADB interface,
// claiming the interface on first use. Returns nil when the module has no ADB
// interface (factory usbcfg) or the claim fails.
//
// The client lives as long as the process: opening a fresh ADB session for
// every call is what wedges this module's adbd, and the gateway does not need
// to — it owns the device for its whole lifetime.
func (m *USBATModem) ADBClient() *adb.Client {
	m.adbMu.Lock()
	defer m.adbMu.Unlock()
	if m.adbClient != nil {
		return m.adbClient
	}
	u := m.currentUSB()
	if u == nil {
		return nil
	}
	if err := u.OpenADB(); err != nil {
		log.Printf("qdc507: adb unavailable: %v", err)
		return nil
	}
	m.adbClient = adb.New(adbTransport{m: m})
	return m.adbClient
}

// RecoverADB revives an adbd that has stopped accepting service opens.
//
// The module's adbd really does wedge: it still answers CNXN, but closes every
// shell stream immediately and never comes back by itself. The cause is
// resource exhaustion, not a USB problem — the firmware has ~43 MB of RAM, and
// its shell service stops being able to fork. A USB reset alone does not clear
// that; a module reboot does.
//
// So the ladder is:
//
//  1. probe with a real command — often nothing is wrong at all
//  2. ask adbd for "reboot:", which forks nothing and therefore still works
//     when every "shell:" open is refused
//  3. if even that cannot be sent, re-enumerate the USB device as a fallback
//  4. wait for the module to come back, open a fresh session, re-arm the AT
//     channel and prove the shell works again
func (m *USBATModem) RecoverADB(ctx context.Context) error {
	m.recoverMu.Lock()
	defer m.recoverMu.Unlock()

	client := m.ADBClient()
	if client == nil {
		return errors.New("qdc507: no adb interface to recover")
	}

	probeCtx, cancelProbe := context.WithTimeout(ctx, 20*time.Second)
	_, probeErr := client.Shell(probeCtx, "true", 15*time.Second)
	cancelProbe()
	if probeErr == nil {
		log.Printf("qdc507: adb answered, nothing to recover")
		return nil
	}
	log.Printf("qdc507: adb is wedged (%v) — rebooting the module", probeErr)

	rebootCtx, cancelReboot := context.WithTimeout(ctx, 20*time.Second)
	rebootErr := client.Reboot(rebootCtx, 15*time.Second)
	cancelReboot()
	if rebootErr != nil {
		log.Printf("qdc507: adb reboot unavailable (%v); re-enumerating USB instead", rebootErr)
		m.usbMu.Lock()
		old := m.usb
		m.usbMu.Unlock()
		if old != nil {
			if err := old.Reset(true); err != nil {
				log.Printf("qdc507: usb re-enumerate: %v", err)
			}
		}
	}

	// The device leaves the bus for a moment and comes back as a new service.
	m.detachUSB()
	client.Reset()

	deadline := time.Now().Add(180 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-m.done:
			return errors.New("qdc507: modem closed during recovery")
		default:
		}
		u, err := openAnyModule()
		if err != nil {
			lastErr = err
			time.Sleep(time.Second)
			continue
		}
		m.usbMu.Lock()
		m.usb = u
		m.usbMu.Unlock()
		if err := u.OpenADB(); err != nil {
			lastErr = err
			m.detachUSB()
			time.Sleep(time.Second)
			continue
		}
		// A fresh adbd is up; the AT side needs its settings again.
		m.drainURC(500 * time.Millisecond)
		checkCtx, cancelCheck := context.WithTimeout(ctx, 20*time.Second)
		_, err = client.Shell(checkCtx, "true", 15*time.Second)
		cancelCheck()
		if err != nil {
			lastErr = err
			client.Reset()
			m.detachUSB()
			time.Sleep(2 * time.Second)
			continue
		}
		if err := m.EnableCallerID(ctx); err != nil {
			log.Printf("qdc507: re-init after recovery: %v", err)
		}
		log.Printf("qdc507: module is back and the adb session is healthy")
		return nil
	}
	return fmt.Errorf("qdc507: module did not come back after reboot: %w", lastErr)
}

// detachUSB closes and forgets the current handle (safe when there is none).
func (m *USBATModem) detachUSB() {
	m.usbMu.Lock()
	u := m.usb
	m.usb = nil
	m.usbMu.Unlock()
	if u != nil {
		_ = u.Close()
	}
}

// openAnyModule opens the module under any of its known vendor ids.
func openAnyModule() (*USBAT, error) {
	var lastErr error
	for _, vid := range QDC507VendorIDs {
		u, err := OpenUSBAT(vid)
		if err == nil {
			return u, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// adbTransport adapts the module's ADB bulk pipes to adb.Transport. It looks
// the USB handle up on every call, so it keeps working across the
// re-enumeration done by RecoverADB.
type adbTransport struct{ m *USBATModem }

func (t adbTransport) Write(p []byte) error {
	// A write can be aborted by the controller (0xe00002eb) when the module's
	// USB gadget is reset — which happens every time the voice route comes up.
	// An aborted bulk transfer sends nothing, so retrying it is safe, and
	// without the retry one flip is enough to lose the whole provisioning run.
	for attempt := 0; ; attempt++ {
		t.m.usbMu.RLock()
		u := t.m.usb
		if u == nil {
			t.m.usbMu.RUnlock()
			return errNoUSBTransport
		}
		err := u.ADBWrite(p)
		t.m.usbMu.RUnlock()
		if err == nil {
			return nil
		}
		if !isTransientUSBError(err) || attempt >= 5 {
			return err
		}
		if attempt == 0 {
			log.Printf("qdc507: adb write aborted, retrying: %v", err)
		}
		time.Sleep(120 * time.Millisecond)
	}
}

func (t adbTransport) Read(p []byte, timeout time.Duration) (int, error) {
	// Chunk the wait so a wedged device cannot hold the read lock (and with it
	// a recovery) for the whole command timeout.
	ms := int(timeout / time.Millisecond)
	if ms > 1000 {
		ms = 1000
	}
	if ms < 1 {
		ms = 1
	}
	deadline := time.Now().Add(timeout)
	for attempt := 0; ; attempt++ {
		t.m.usbMu.RLock()
		u := t.m.usb
		if u == nil {
			t.m.usbMu.RUnlock()
			return 0, errNoUSBTransport
		}
		n, err := u.ADBRead(p, ms)
		t.m.usbMu.RUnlock()
		if err == nil {
			return n, nil
		}
		// The controller aborts an idle bulk read on this module from time to
		// time (kIOReturnAborted). It says nothing about the ADB session — a
		// command that is simply quiet for a few seconds provokes it — so ride
		// it out instead of failing the command.
		if !isTransientUSBError(err) || time.Now().After(deadline) {
			return 0, err
		}
		if attempt == 3 {
			log.Printf("qdc507: adb read aborted, retrying: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// isTransientUSBError reports whether a USB error is worth retrying.
func isTransientUSBError(err error) bool {
	msg := err.Error()
	for _, code := range []string{
		"0xe00002eb", // kIOReturnAborted
		"0xe00002d6", // kIOReturnNotResponding
		"0xe0004051", // timeout-ish
	} {
		if strings.Contains(msg, code) {
			return true
		}
	}
	return false
}

// AudioDevice: the QDC507 exposes no USB Audio interface (see hardware spike),
// so the gateway cannot yet bridge RTP to the module's voice path.
func (m *USBATModem) AudioDevice() string { return "" }

// --- reader goroutines -----------------------------------------------------

func (m *USBATModem) readAT() {
	m.readLoop(m.readATChunk, m.handleATLine)
}

func (m *USBATModem) readURC() {
	m.readLoop(m.readURCChunk, m.handleURCLine)
}

func (m *USBATModem) readLoop(read func([]byte, int) (int, error), handle func(string)) {
	buf := make([]byte, 4096)
	var partial string
	var lastErrLog time.Time
	for {
		select {
		case <-m.done:
			return
		default:
		}
		n, err := read(buf, 500)
		if err != nil {
			// A read that is aborted at the USB layer happens from time to time
			// (0xe00002eb); do not spam the log with it.
			if time.Since(lastErrLog) > 60*time.Second {
				log.Printf("qdc507: usb read: %v", err)
				lastErrLog = time.Now()
			}
			time.Sleep(150 * time.Millisecond)
			continue
		}
		if n == 0 {
			continue
		}
		partial += string(buf[:n])
		for {
			// The AT+CMGS ">" prompt has no trailing newline; flush it immediately
			// so SendSMS can detect it before the read timeout.
			if strings.Contains(partial, ">") && !strings.Contains(partial, "\n") {
				trimmed := strings.TrimSpace(partial)
				if trimmed == ">" || strings.HasSuffix(trimmed, ">") {
					handle(">")
					partial = ""
					break
				}
			}
			i := strings.IndexByte(partial, '\n')
			if i < 0 {
				break
			}
			line := strings.TrimRight(partial[:i], "\r")
			partial = partial[i+1:]
			// Also catch ">" that appears at the start of a line (some modems send "\r\n> ")
			if strings.TrimSpace(line) == ">" {
				handle(">")
				continue
			}
			if line != "" {
				handle(line)
			}
		}
	}
}

// handleATLine routes a line read from the AT port: URCs are handled as events,
// everything else is a response to the command currently in flight.
func (m *USBATModem) handleATLine(line string) {
	if isURC(line) {
		m.handleURCLine(line)
		return
	}
	select {
	case m.atLines <- line:
	default: // drop rather than block the reader
	}
}

func isURC(line string) bool {
	switch {
	case line == "RING", line == "NO CARRIER", line == "BUSY", line == "NO ANSWER":
		return true
	case strings.HasPrefix(line, "+CLIP:"),
		strings.HasPrefix(line, "+CMTI:"),
		strings.HasPrefix(line, "+CMT:"),
		strings.HasPrefix(line, "+CREG:"),
		strings.HasPrefix(line, "+CFUN:"),
		strings.HasPrefix(line, "+CPIN:"),
		strings.HasPrefix(line, "+QIND:"):
		return true
	}
	return false
}

func (m *USBATModem) handleURCLine(line string) {
	switch {
	case line == "RING":
		// A real incoming call makes the module emit RING roughly every
		// 4–6 seconds until it is answered or the caller gives up. Only the
		// first one starts a call; the repeats must not spawn extra INVITEs.
		m.stateMu.Lock()
		if m.ringing {
			m.stateMu.Unlock()
			return
		}
		m.ringing = true
		m.stateMu.Unlock()
		m.startCallPoll()

		// Safety net: if the module never sends NO CARRIER (some networks just
		// stop ringing), do not stay "ringing" forever or the *next* call is
		// swallowed as a repeat.
		time.AfterFunc(90*time.Second, m.clearRinging)

		// +CLIP usually follows RING a moment later; wait briefly for it.
		m.stateMu.Lock()
		if m.ringTimer != nil {
			m.ringTimer.Stop()
		}
		m.ringTimer = time.AfterFunc(1200*time.Millisecond, func() {
			m.stateMu.Lock()
			num := strings.TrimSpace(m.pendingCLIP)
			m.pendingCLIP = ""
			m.ringTimer = nil
			m.stateMu.Unlock()
			m.emit(Event{Type: EventIncomingCall, Number: num, At: time.Now()})
		})
		m.stateMu.Unlock()

	case strings.HasPrefix(line, "+CLIP:"):
		num := parseQuoted(line)
		m.stateMu.Lock()
		if !m.ringing {
			// Stale +CLIP without a RING (e.g. left over in the module buffer).
			m.stateMu.Unlock()
			return
		}
		m.pendingCLIP = num
		timer := m.ringTimer
		m.ringTimer = nil
		m.stateMu.Unlock()
		if timer != nil {
			timer.Stop()
			m.emit(Event{Type: EventIncomingCall, Number: num, At: time.Now()})
		}

	case strings.HasPrefix(line, "+CMTI:"):
		// +CMTI: "SM",12
		idx := -1
		if i := strings.LastIndex(line, ","); i >= 0 {
			if v, err := strconv.Atoi(strings.TrimSpace(line[i+1:])); err == nil {
				idx = v
			}
		}
		m.emit(Event{Type: EventIncomingSMS, Index: idx, At: time.Now()})

	case line == "NO CARRIER", line == "BUSY", line == "NO ANSWER":
		m.clearRinging()
		m.emit(Event{Type: EventCallEnded, Number: line, At: time.Now()})
	}
}

// clearRinging ends the "a call is coming in" state, so the next RING is
// treated as a new call.
func (m *USBATModem) clearRinging() {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	m.ringing = false
	m.dialing = false
	m.pendingCLIP = ""
	if m.ringTimer != nil {
		m.ringTimer.Stop()
		m.ringTimer = nil
	}
}

func (m *USBATModem) emit(ev Event) {
	select {
	case m.urcs <- ev:
	case <-m.done:
	default:
		log.Printf("qdc507: urc buffer full, dropping %s", ev.Type)
	}
}

// parseNthQuoted extracts the n-th (1-based) double-quoted value from a line.
// For +CMGR: "REC UNREAD","+8613800138000",...  n=2 returns +8613800138000.
func parseNthQuoted(line string, n int) string {
	count := 0
	rest := line
	for count < n {
		a := strings.IndexByte(rest, '"')
		if a < 0 {
			return ""
		}
		b := strings.IndexByte(rest[a+1:], '"')
		if b < 0 {
			return ""
		}
		count++
		if count == n {
			return rest[a+1 : a+1+b]
		}
		rest = rest[a+1+b+1:]
	}
	return ""
}

func parseQuoted(line string) string {
	a := strings.IndexByte(line, '"')
	if a < 0 {
		return ""
	}
	b := strings.IndexByte(line[a+1:], '"')
	if b < 0 {
		return ""
	}
	return line[a+1 : a+1+b]
}

// --- command plumbing ------------------------------------------------------

var errATTimeout = errors.New("qdc507: AT command timed out")

// command writes one AT command and collects the response up to OK/ERROR.
func (m *USBATModem) command(ctx context.Context, timeout time.Duration, cmd string) (string, error) {
	m.cmdMu.Lock()
	defer m.cmdMu.Unlock()
	return m.commandLocked(ctx, timeout, cmd)
}

// commandLocked is command with cmdMu already held.
func (m *USBATModem) commandLocked(ctx context.Context, timeout time.Duration, cmd string) (string, error) {
	// ESC first: if a previous AT+CMGS was abandoned, the module is still
	// waiting for a message body and would swallow this command as text.
	// Sending ESC aborts that dangling prompt (standard Hayes behaviour); on an
	// idle line it is a no-op.
	_ = m.writeAT([]byte("\x1b"))

	// Drop anything stale from a previous transaction.
	for {
		select {
		case <-m.atLines:
			continue
		default:
		}
		break
	}

	if err := m.writeAT([]byte(cmd + "\r")); err != nil {
		return "", err
	}

	deadline := time.Now().Add(timeout)
	var out strings.Builder
	echo := cmd
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return out.String(), errATTimeout
		}
		select {
		case <-ctx.Done():
			return out.String(), ctx.Err()
		case <-time.After(remaining):
			return out.String(), errATTimeout
		case line := <-m.atLines:
			switch {
			case line == echo:
				// echo of our own command
			case line == "OK":
				return out.String(), nil
			case line == "ERROR":
				return out.String(), fmt.Errorf("qdc507: %s -> ERROR", cmd)
			case strings.HasPrefix(line, "+CME ERROR"), strings.HasPrefix(line, "+CMS ERROR"):
				return out.String(), fmt.Errorf("qdc507: %s -> %s", cmd, line)
			default:
				out.WriteString(line)
				out.WriteByte('\n')
			}
		}
	}
}

// --- Driver ---------------------------------------------------------------

func (m *USBATModem) Events(ctx context.Context) (<-chan Event, error) {
	out := make(chan Event, 32)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case <-m.done:
				return
			case ev := <-m.urcs:
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

func (m *USBATModem) EnableCallerID(ctx context.Context) error {
	init := []string{
		"AT",                // sanity
		"AT+CLIP=1",         // caller ID on incoming calls
		"AT+CMGF=1",         // SMS text mode
		"AT+CNMI=2,1,0,0,0", // deliver +CMTI to us
	}
	for _, cmd := range init {
		if _, err := m.command(ctx, 5*time.Second, cmd); err != nil {
			return fmt.Errorf("init %s: %w", cmd, err)
		}
	}
	return nil
}

func (m *USBATModem) Dial(ctx context.Context, number string) error {
	m.clearRinging()
	m.setState(func(s *modemState) { s.dialing = true })
	m.startCallPoll()
	_, err := m.command(ctx, 20*time.Second, "ATD"+number+";")
	return err
}

// startCallPoll watches AT+CLCC while a call is up.
//
// The module never tells us that an outgoing call was answered — no URC reports
// it, and this driver used to emit EventCallConnected only in the fake modem,
// so a call placed from the phone stayed "ringing" on the phone's side forever:
// no 200 OK, no audio bridge, no sound. Both reference implementations poll
// AT+CLCC for exactly this reason. A CLCC entry with mode 0 (voice) and
// state 0 (active) is the answer.
func (m *USBATModem) startCallPoll() {
	m.callPollMu.Lock()
	if m.callPolling {
		m.callPollMu.Unlock()
		return
	}
	m.callPolling = true
	m.callPollMu.Unlock()

	go func() {
		defer func() {
			m.callPollMu.Lock()
			m.callPolling = false
			m.callPollMu.Unlock()
		}()
		reported := false
		activeStreak := 0
		for i := 0; i < 900; i++ {
			if !m.callInProgress() {
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			out, err := m.command(ctx, 3*time.Second, "AT+CLCC")
			cancel()
			if err == nil {
				state, ok := clccVoiceState(out)
				// Two consecutive active readings: right after ATD the module
				// can still be showing the previous call's entry, and a false
				// "answered" sends the phone a 200 OK for a call nobody picked
				// up (then NO CARRIER drops it a second later).
				if ok && state == 0 {
					activeStreak++
					if activeStreak >= 2 && !reported {
						reported = true
						log.Printf("qdc507: call is active (CLCC)")
						m.emit(Event{Type: EventCallConnected, At: time.Now()})
					}
				} else {
					activeStreak = 0
				}
			}
			time.Sleep(900 * time.Millisecond)
		}
	}()
}

// clccVoiceState returns the state of the first voice call in an AT+CLCC
// response (0 = active, 3 = alerting, 4 = incoming …). Data sessions
// (mode 1) are ignored — this module keeps LTE bearers listed.
func clccVoiceState(raw string) (int, bool) {
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "+CLCC:") {
			continue
		}
		fields := strings.Split(strings.TrimPrefix(line, "+CLCC:"), ",")
		if len(fields) < 4 {
			continue
		}
		state, err1 := strconv.Atoi(strings.TrimSpace(fields[2]))
		mode, err2 := strconv.Atoi(strings.TrimSpace(fields[3]))
		if err1 != nil || err2 != nil {
			continue
		}
		if mode != 0 { // 0 = voice, 1 = data
			continue
		}
		return state, true
	}
	return 0, false
}

func (m *USBATModem) setState(f func(*modemState)) {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	s := modemState{ringing: m.ringing, dialing: m.dialing}
	f(&s)
	m.ringing, m.dialing = s.ringing, s.dialing
}

type modemState struct {
	ringing bool
	dialing bool
}

func (m *USBATModem) callInProgress() bool {
	m.stateMu.Lock()
	defer m.stateMu.Unlock()
	return m.ringing || m.dialing
}

func (m *USBATModem) Answer(ctx context.Context) error {
	m.clearRinging()
	_, err := m.command(ctx, 10*time.Second, "ATA")
	return err
}

func (m *USBATModem) Hangup(ctx context.Context) error {
	m.clearRinging()
	_, err := m.command(ctx, 10*time.Second, "ATH")
	return err
}

// isASCII returns true if every byte in s is in the ASCII range.
func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > 127 {
			return false
		}
	}
	return true
}

// toUCS2Hex encodes a UTF-8 string as a UCS-2 big-endian hex string (e.g.
// "4F60597D" for Chinese characters). This is what AT+CSCS="UCS2" expects.
func toUCS2Hex(s string) string {
	runes := []rune(s)
	u16 := utf16.Encode(runes)
	buf := make([]byte, len(u16)*2)
	for i, v := range u16 {
		buf[i*2] = byte(v >> 8)
		buf[i*2+1] = byte(v)
	}
	return strings.ToUpper(hex.EncodeToString(buf))
}

// phoneToUCS2Hex encodes a phone number (ASCII digits and +) as UCS-2 hex.
// When CSCS=UCS2 the modem expects the destination number in UCS2 as well.
func phoneToUCS2Hex(num string) string {
	return toUCS2Hex(num)
}

// SendSMS uses the AT+CMGS two-step handshake: the module answers with a ">"
// prompt, then the body is written and terminated with Ctrl-Z.
//
// For non-ASCII text (Chinese, emoji, etc.) the charset is switched to UCS2
// and both the number and body are hex-encoded. This mirrors what CellBridge
// and every other QDC507 implementation does for international SMS.
func (m *USBATModem) SendSMS(ctx context.Context, to, text string) error {
	m.cmdMu.Lock()
	defer m.cmdMu.Unlock()

	useUCS2 := !isASCII(text)
	log.Printf("qdc507: SendSMS to=%q text=%q ucs2=%v", to, text, useUCS2)

	if _, err := m.commandLocked(ctx, 5*time.Second, "AT+CMGF=1"); err != nil {
		return err
	}

	if useUCS2 {
		if _, err := m.commandLocked(ctx, 5*time.Second, `AT+CSCS="UCS2"`); err != nil {
			return err
		}
	} else {
		// Ensure we are in a known charset for plain ASCII messages.
		if _, err := m.commandLocked(ctx, 5*time.Second, `AT+CSCS="GSM"`); err != nil {
			return err
		}
	}

	dest := to
	if useUCS2 {
		dest = phoneToUCS2Hex(to)
	}
	if err := m.writeAT([]byte(`AT+CMGS="` + dest + "\"\r")); err != nil {
		return err
	}

	// Wait for the ">" prompt.
	promptDeadline := time.Now().Add(10 * time.Second)
	for {
		remaining := time.Until(promptDeadline)
		if remaining <= 0 {
			_ = m.writeAT([]byte("\x1b"))
			return errATTimeout
		}
		select {
		case <-ctx.Done():
			_ = m.writeAT([]byte("\x1b"))
			return ctx.Err()
		case <-time.After(remaining):
			_ = m.writeAT([]byte("\x1b"))
			return errATTimeout
		case line := <-m.atLines:
			log.Printf("qdc507: SendSMS prompt-wait got line: %q", line)
			if strings.Contains(line, ">") || strings.HasPrefix(line, ">") {
				goto sendBody
			}
			if strings.HasPrefix(line, "+CMS ERROR") || strings.HasPrefix(line, "+CME ERROR") ||
				line == "ERROR" {
				_ = m.writeAT([]byte("\x1b"))
				return fmt.Errorf("qdc507: CMGS rejected: %s", line)
			}
		}
	}

sendBody:
	body := text
	if useUCS2 {
		body = toUCS2Hex(text)
	}
	if err := m.writeAT([]byte(body + "\x1a")); err != nil {
		return err
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return errATTimeout
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(remaining):
			return errATTimeout
		case line := <-m.atLines:
			log.Printf("qdc507: SendSMS body-wait got line: %q", line)
			if line == "OK" || strings.HasPrefix(line, "+CMGS:") {
				// Restore GSM charset so ReadSMS and other commands work normally.
				if useUCS2 {
					_, _ = m.commandLocked(ctx, 3*time.Second, `AT+CSCS="GSM"`)
				}
				log.Printf("qdc507: SendSMS succeeded")
				return nil
			}
			if strings.HasPrefix(line, "+CMS ERROR") || strings.HasPrefix(line, "+CME ERROR") ||
				line == "ERROR" {
				if useUCS2 {
					_, _ = m.commandLocked(ctx, 3*time.Second, `AT+CSCS="GSM"`)
				}
				return fmt.Errorf("qdc507: send failed: %s", line)
			}
		}
	}
}

func (m *USBATModem) ReadSMS(ctx context.Context, index int) (from, text string, err error) {
	// Ensure charset is GSM so we get readable text, not UCS2 hex.
	_, _ = m.command(ctx, 5*time.Second, `AT+CSCS="GSM"`)
	_, _ = m.command(ctx, 5*time.Second, "AT+CMGF=1")

	// The module sometimes needs a moment after +CMTI before the message
	// is readable. Retry up to 3 times with a short delay.
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(500 * time.Millisecond)
		}
		raw, err := m.command(ctx, 10*time.Second, fmt.Sprintf("AT+CMGR=%d", index))
		if err != nil {
			log.Printf("qdc507: ReadSMS attempt %d error: %v", attempt+1, err)
			continue
		}
		log.Printf("qdc507: CMGR attempt %d raw: %q", attempt+1, raw)
		// +CMGR: "REC UNREAD","+8613800138000",,"24/05/01,12:00:00+32"
		// body lines follow.
		lines := strings.Split(strings.TrimRight(raw, "\n"), "\n")
		for i, line := range lines {
			if strings.HasPrefix(line, "+CMGR:") {
				from = parseNthQuoted(line, 2) // 2nd quoted field = phone number
				if i+1 < len(lines) {
					text = strings.TrimSpace(lines[i+1])
				}
				break
			}
		}
		if from != "" || text != "" {
			log.Printf("qdc507: ReadSMS index=%d from=%q text=%q", index, from, text)
			return from, text, nil
		}
		log.Printf("qdc507: ReadSMS attempt %d: empty response, retrying", attempt+1)
	}
	log.Printf("qdc507: ReadSMS index=%d failed after 3 attempts", index)
	return "", "", nil
}

func (m *USBATModem) DeleteSMS(ctx context.Context, index int) error {
	_, err := m.command(ctx, 10*time.Second, fmt.Sprintf("AT+CMGD=%d", index))
	return err
}

func (m *USBATModem) Signal(ctx context.Context) (int, error) {
	raw, err := m.command(ctx, 8*time.Second, "AT+CSQ")
	if err != nil {
		return 99, err
	}
	i := strings.Index(raw, "+CSQ:")
	if i < 0 {
		return 99, errors.New("qdc507: no +CSQ in response")
	}
	rest := strings.TrimSpace(raw[i+len("+CSQ:"):])
	if j := strings.IndexByte(rest, ','); j >= 0 {
		rest = rest[:j]
	}
	v, err := strconv.Atoi(strings.TrimSpace(rest))
	if err != nil {
		return 99, err
	}
	return v, nil
}
