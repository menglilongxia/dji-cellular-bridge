// Package gateway wires the modem, SIP server, SMS engine, push service
// and HTTP API together into the call/SMS forwarding logic.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/dji-cellular-bridge/mac-gateway/internal/adb"
	"github.com/dji-cellular-bridge/mac-gateway/internal/modem"
	"github.com/dji-cellular-bridge/mac-gateway/internal/push"
	"github.com/dji-cellular-bridge/mac-gateway/internal/sip"
	"github.com/dji-cellular-bridge/mac-gateway/internal/history"
	"github.com/dji-cellular-bridge/mac-gateway/internal/sms"
	"github.com/dji-cellular-bridge/mac-gateway/internal/voiceroute"
)

// Config for the gateway controller.
type Config struct {
	Modem       string // "fake" or a serial port path
	SIPAddr     string
	AdvertiseIP string
	Username    string
	Password    string
	LoopbackRTP bool

	FakeCallAfter time.Duration
	FakeCaller    string
	FakeSMSAfter  time.Duration

	// CoreAudio bridges call audio to the module's USB Audio (UAC) interface
	// instead of echoing RTP back. Requires the module's usbcfg composition to
	// have UAC enabled — see docs/HARDWARE-SPIKE.md.
	CoreAudio   bool
	AudioDevice string

	// VoiceScript is run as "<script> start" when a call becomes active and
	// "<script> stop" when it ends. It provisions the module-side VoLTE voice
	// route (kernel modules + alsaucm calibration + mavo-pcm-bridge) — without
	// it the call connects but carries no audio.
	VoiceScript string

	// VoiceADB provisions the module-side VoLTE route through the gateway's own
	// ADB channel instead of shelling out to a script. This is the supported
	// path: an external ADB tool has to seize the USB device from the gateway,
	// which breaks the AT channel and wedges the module's adbd.
	VoiceADB bool
	// VoiceFilesDir is where the vendor runtime lives on this Mac
	// (~/Library/Application Support/CellBridge/module-voice).
	VoiceFilesDir string
	// ModuleIP is the module's address on its USB ECM link (192.168.225.1).
	ModuleIP string
	// DataDir is where persistent state (history, etc.) is stored.
	DataDir string
}

// Controller owns all components and routes events between them.
type Controller struct {
	cfg   Config
	modem modem.Driver
	sip   *sip.Server
	sms   *sms.Engine
	push  push.Service

	mu             sync.Mutex
	pending        map[string]*sip.Session // session ID -> outgoing calls waiting for modem
	callHistoryIDs map[string]string        // session ID -> history record ID

	audio   *audioBridge
	history *history.Store

	// voice provisions the module-side voice route over ADB (nil when the
	// script fallback is in use).
	voice *voiceroute.Provisioner
	// adbProvider is the modem driver's ADB channel; the provisioner is built
	// from it lazily, so a module without an ADB interface does not stop the
	// gateway from placing calls.
	adbProvider adbProvider
	voiceErr    error
	// adbRecover can reset the USB device when adbd wedges.
	adbRecover adbRecoverer
}

// adbProvider is implemented by the real modem driver.
type adbProvider interface {
	ADBClient() *adb.Client
}

// adbRecoverer is implemented by the real modem driver; it is what lets the
// gateway recover from a module whose adbd stopped accepting service opens.
type adbRecoverer interface {
	RecoverADB(ctx context.Context) error
}

func NewController(cfg Config, pushSvc push.Service) (*Controller, error) {
	var drv modem.Driver
	switch cfg.Modem {
	case "fake":
		drv = modem.NewFakeModem(modem.FakeOptions{
			IncomingCallAfter: cfg.FakeCallAfter,
			IncomingCaller:    cfg.FakeCaller,
			IncomingSMSAfter:  cfg.FakeSMSAfter,
		})
	case "usb", "qdc507":
		// Real DJI QDC507 over the user-space USB AT transport
		// (macOS has no /dev/tty.* for this module — see docs/HARDWARE-SPIKE.md).
		m, err := modem.NewUSBATModem()
		if err != nil {
			return nil, fmt.Errorf("open qdc507 module: %w", err)
		}
		drv = m
	default:
		drv = modem.NewSerialModem(cfg.Modem)
	}
	c := &Controller{
		cfg:     cfg,
		modem:   drv,
		sms:     sms.NewEngine(drv),
		push:    pushSvc,
		pending:        make(map[string]*sip.Session),
		callHistoryIDs: make(map[string]string),
	}
	if cfg.VoiceADB {
		provider, ok := drv.(adbProvider)
		if !ok {
			return nil, fmt.Errorf("voice-adb: %s driver has no ADB channel", cfg.Modem)
		}
		c.adbProvider = provider
		if r, ok := drv.(adbRecoverer); ok {
			c.adbRecover = r
		}
		if provider.ADBClient() == nil {
			// Not fatal: the gateway still places calls and forwards SMS. The
			// voice route simply cannot be provisioned until the module shows
			// an ADB interface, and every /api/v1/voice call says so.
			log.Printf("voice: module exposes no ADB interface — calls will carry no audio until it does")
		}
	}
	name := cfg.AudioDevice
	if name == "" {
		name = "BAIWANG"
	}
	c.audio = newAudioBridge(cfg.CoreAudio, name)
	dataDir := cfg.DataDir
	if dataDir == "" {
		dataDir = "data"
	}
	c.history = history.NewStore(dataDir)
	srv := sip.NewServer(sip.Config{
		Addr:        cfg.SIPAddr,
		Username:    cfg.Username,
		Password:    cfg.Password,
		AdvertiseIP: cfg.AdvertiseIP,
		LoopbackRTP: cfg.LoopbackRTP,
	}, sip.Callbacks{
		OnOutgoingCall: c.onOutgoingCall,
		OnAnswered:     c.onAnswered,
		OnEnded:        c.onEnded,
		OnMessage:      c.onClientMessage,
		OnRegistered:   func(b *sip.Binding) {},
	})
	c.sip = srv
	c.sms.OnIncoming = c.onIncomingSMS
	return c, nil
}

// Run starts all components; returns when ctx is done.
func (c *Controller) Run(ctx context.Context) error {
	if err := c.modem.EnableCallerID(ctx); err != nil {
		return fmt.Errorf("modem init: %w", err)
	}
	events, err := c.modem.Events(ctx)
	if err != nil {
		return fmt.Errorf("modem events: %w", err)
	}
	go c.sip.Run(ctx) //nolint:errcheck
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-events:
				if !ok {
					return
				}
				c.handleModemEvent(ctx, ev)
			}
		}
	}()
	<-ctx.Done()
	return nil
}

func (c *Controller) SIP() *sip.Server  { return c.sip }
func (c *Controller) SMS() *sms.Engine  { return c.sms }
func (c *Controller) ModemName() string { return c.modem.Name() }

// --- modem -> phone ---

func (c *Controller) handleModemEvent(ctx context.Context, ev modem.Event) {
	log.Printf("modem event: %s number=%s", ev.Type, ev.Number)
	switch ev.Type {
	case modem.EventIncomingCall:
		callID := c.history.AddCall("in", ev.Number, "ringing")
		c.voiceStart()
		go c.ringPhone(ctx, ev.Number, callID)
	case modem.EventCallConnected:
		c.mu.Lock()
		var sess *sip.Session
		for id, s := range c.pending {
			sess = s
			delete(c.pending, id)
			break
		}
		c.mu.Unlock()
		if sess != nil {
			sess.Accept() // 200 OK -> phone
		}
	case modem.EventCallEnded:
		for _, s := range c.sip.ActiveSessions() {
			s.Bye()
		}
		c.mu.Lock()
		c.pending = make(map[string]*sip.Session)
		c.mu.Unlock()
	case modem.EventIncomingSMS:
		if _, err := c.sms.HandleModemSMS(ctx, ev.Index); err != nil {
			log.Printf("sms read failed: %v", err)
		}
	}
}

// ringPhone: push + SIP INVITE in parallel (mirrors CellBridge).
// callID is the history record for this incoming call; ringPhone updates its
// status to "answered" or "missed" when the outcome is known.
// The app may be waking from the VoIP push, so INVITE is retried for up to
// ~60 s to give the user time to open the app after reading the SMS alert.
func (c *Controller) ringPhone(ctx context.Context, number, callID string) {
	token := c.sip.PushToken()
	// Send VoIP push or SMS alert (whichever is configured).
	_ = c.push.SendVoIP(ctx, token, number, "sip:"+number+"@gateway")

	var sess *sip.Session
	var err error
	// Up to 12 attempts × 5 s per attempt = 60 s total; each attempt is cheap
	// (5 s) so we react quickly when the app is already open.
	for attempt := 0; attempt < 12; attempt++ {
		callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		sess, err = c.sip.InviteClient(callCtx, number)
		cancel()
		if err == nil || !errors.Is(err, sip.ErrNoClient) {
			break
		}
		if attempt == 0 {
			log.Printf("ringPhone: no registered client yet, retrying for up to 60s…")
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
	if err != nil {
		log.Printf("incoming call from %s missed: %v", number, err)
		c.history.UpdateCallStatus(callID, "missed")
		_ = c.push.SendMissedCall(ctx, token, number)
		return
	}
	// App answered: update history and connect the modem voice path.
	c.history.UpdateCallStatus(callID, "answered")
	if err := c.modem.Answer(ctx); err != nil {
		log.Printf("modem answer failed: %v", err)
		sess.Bye()
	}
}

func (c *Controller) onIncomingSMS(m sms.Message) {
	c.history.AddSMS("in", m.Peer, m.Body, "received")
	token := c.sip.PushToken()
	if token != "" {
		_ = c.push.SendMessage(context.Background(), token, m.Peer, m.Body)
	}
	// Best effort: deliver via SIP MESSAGE too (works when the app is online).
	if err := c.sip.SendMessageToClient(m.Peer, m.Body); err != nil {
		log.Printf("sip message to client failed: %v", err)
	}
}

// --- phone -> modem ---

func (c *Controller) onOutgoingCall(sess *sip.Session) {
	hid := c.history.AddCall("out", sess.Number, "dialing")
	c.mu.Lock()
	c.pending[sess.ID] = sess
	c.callHistoryIDs[sess.ID] = hid
	c.mu.Unlock()
	c.voiceStart()
	if err := c.modem.Dial(context.Background(), sess.Number); err != nil {
		log.Printf("modem dial failed: %v", err)
		c.mu.Lock()
		delete(c.pending, sess.ID)
		delete(c.callHistoryIDs, sess.ID)
		c.mu.Unlock()
		c.history.UpdateCallStatus(hid, "failed")
		sess.Terminate("dial-failed")
	}
}

func (c *Controller) onAnswered(sess *sip.Session) {
	log.Printf("call %s (%s) active, rtp :%d", sess.ID, sess.Number, sess.RTPPort())
	c.mu.Lock()
	hid := c.callHistoryIDs[sess.ID]
	c.mu.Unlock()
	if hid != "" {
		c.history.UpdateCallStatus(hid, "answered")
	}
	c.voiceEnsure()
	c.audio.start(sess)
	c.voiceWatchdog()
}

func (c *Controller) onEnded(sess *sip.Session, reason string) {
	c.mu.Lock()
	hid := c.callHistoryIDs[sess.ID]
	delete(c.pending, sess.ID)
	delete(c.callHistoryIDs, sess.ID)
	c.mu.Unlock()
	if hid != "" {
		c.history.EndCall(hid)
	}
	c.audio.stop()
	// The module-side route is deliberately left running between calls.
	// CellBridge's hardware notes: killing the mavo session per call wedged the
	// UAC stream into silence, and their verified flow only tears down the
	// *host* audio pair — the next call rotates the module route anyway, which
	// is what revives the gadget's capture stream.
	_ = c.modem.Hangup(context.Background()) // idempotent-ish; fake modem emits CallEnded -> no active sessions -> harmless
}

func (c *Controller) onClientMessage(from, to, body string) {
	c.history.AddSMS("out", to, body, "sending")
	if _, err := c.sms.Send(context.Background(), to, body); err != nil {
		log.Printf("sms send failed: %v", err)
	}
}

// --- API helpers ---

// Status for GET /api/v1/status.
func (c *Controller) Status(ctx context.Context) map[string]any {
	rssi, _ := c.modem.Signal(ctx)
	sessions := []map[string]any{}
	for _, s := range c.sip.ActiveSessions() {
		sessions = append(sessions, map[string]any{
			"id": s.ID, "direction": string(s.Direction),
			"number": s.Number, "state": string(s.State), "rtp_port": s.RTPPort(),
		})
	}
	return map[string]any{
		"modem":      c.modem.Name(),
		"signal":     rssi,
		"registered": c.sip.HasBinding(),
		"calls":      sessions,
	}
}

// SendSMS via HTTP API.
func (c *Controller) SendSMS(ctx context.Context, to, body string) (*sms.Message, error) {
	return c.sms.Send(ctx, to, body)
}

// History returns the history store.
func (c *Controller) History() *history.Store { return c.history }

// SetPushService replaces the push service after construction.
// Must be called before Run() starts goroutines.
func (c *Controller) SetPushService(svc push.Service) { c.push = svc }

// SendSMSViaModem sends a raw SMS through the modem.
// Exposed for the SMSAlertService so it can share the modem driver.
func (c *Controller) SendSMSViaModem(ctx context.Context, to, text string) error {
	return c.modem.SendSMS(ctx, to, text)
}


// FakeIncomingCall triggers a fake incoming call (dev only).
func (c *Controller) FakeIncomingCall(from string) error {
	f, ok := c.modem.(*modem.FakeModem)
	if !ok {
		return errors.New("not a fake modem (run with --modem=fake)")
	}
	f.TriggerIncomingCall(from)
	return nil
}

// FakeIncomingSMS triggers a fake incoming SMS (dev only).
func (c *Controller) FakeIncomingSMS(from, body string) error {
	f, ok := c.modem.(*modem.FakeModem)
	if !ok {
		return errors.New("not a fake modem (run with --modem=fake)")
	}
	f.TriggerIncomingSMS(from, body)
	return nil
}
