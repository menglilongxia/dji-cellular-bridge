// gatewayd is the DJI cellular bridge gateway daemon.
//
// It bridges a 4G module (DJI QDC507, or a fake one for development) to an
// iPhone app over SIP/RTP: calls, SMS, and push wake-ups.
//
// Usage:
//
//	gatewayd --modem=fake --sip=:5060 --api=:8787 --user=iphone --pass=changeme
//	gatewayd --modem=fake --fake-call-after=20s --fake-sms-after=10s  # scripted demo
package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dji-cellular-bridge/mac-gateway/internal/api"
	"github.com/dji-cellular-bridge/mac-gateway/internal/gateway"
	"github.com/dji-cellular-bridge/mac-gateway/internal/push"
)

// echoUDP answers every datagram with "GWMAC" and logs the sender, so a
// diagnostic from the phone is visible on both ends.
func echoUDP(addr string) {
	conn, err := net.ListenPacket("udp4", addr)
	if err != nil {
		log.Printf("udp echo: %v", err)
		return
	}
	buf := make([]byte, 2048)
	for {
		n, from, err := conn.ReadFrom(buf)
		if err != nil {
			return
		}
		log.Printf("udp echo: %d bytes from %s (%s)", n, from, string(buf[:min(n, 32)]))
		_, _ = conn.WriteTo([]byte("GWMAC"), from)
	}
}

func main() {
	modemFlag := flag.String("modem", "fake", "modem driver: \"fake\" or serial port path")
	sipAddr := flag.String("sip", ":5060", "SIP UDP listen address")
	apiAddr := flag.String("api", ":8787", "HTTP API listen address")
	apiToken := flag.String("api-token", "", "Bearer token for HTTP API (empty = no auth, dev only)")
	advertise := flag.String("advertise-ip", "", "IP advertised in SDP/Via (default: auto-detect)")
	user := flag.String("user", "iphone", "SIP username")
	pass := flag.String("pass", "changeme", "SIP password")
	loopback := flag.Bool("loopback-rtp", true, "echo RTP (fake audio, no hardware)")
	coreAudio := flag.Bool("coreaudio", false, "bridge call audio to the module's USB Audio interface (needs UAC enabled in the module's usbcfg)")
	audioDevice := flag.String("audio-device", "BAIWANG", "substring of the CoreAudio device name/manufacturer to use")
	voiceScript := flag.String("voice-script", "", "module-side voice provisioning script (run with start/stop per call)")
	voiceADB := flag.Bool("voice-adb", false, "provision the module-side voice route through the gateway's own ADB channel (preferred; needs --voice-files)")
	voiceFiles := flag.String("voice-files", "", "directory holding the vendor voice runtime (qdc507_aprv3.ko, qdc507_voice.ko, mavo-pcm-bridge.armv7)")
	moduleIP := flag.String("module-ip", "192.168.225.1", "the module's address on its USB ECM link (used to push the voice runtime)")
	dataDir := flag.String("data-dir", "", "directory for persistent data (history, etc.); defaults to ~/Library/Application Support/CellBridge")
	fakeCallAfter := flag.Duration("fake-call-after", 0, "fake modem: incoming call after this delay (demo)")
	fakeCaller := flag.String("fake-caller", "13800138000", "fake modem: incoming caller number")
	fakeSMSAfter := flag.Duration("fake-sms-after", 0, "fake modem: incoming SMS after this delay (demo)")
	alertPhone := flag.String("alert-phone", "", "phone number (E.164) to SMS-alert when the app is not running (free-tier push fallback)")
	flag.Parse()

	// Default data dir
	dataDirVal := *dataDir
	if dataDirVal == "" {
		home, _ := os.UserHomeDir()
		dataDirVal = home + "/Library/Application Support/CellBridge"
	}

	ctrl, err := gateway.NewController(gateway.Config{
		Modem:         *modemFlag,
		SIPAddr:       *sipAddr,
		AdvertiseIP:   *advertise,
		Username:      *user,
		Password:      *pass,
		LoopbackRTP:   *loopback,
		CoreAudio:     *coreAudio,
		AudioDevice:   *audioDevice,
		VoiceScript:   *voiceScript,
		VoiceADB:      *voiceADB,
		VoiceFilesDir: *voiceFiles,
		ModuleIP:      *moduleIP,
		DataDir:       dataDirVal,
		FakeCallAfter: *fakeCallAfter,
		FakeCaller:    *fakeCaller,
		FakeSMSAfter:  *fakeSMSAfter,
	}, push.ConsoleService{})
	if err != nil {
		log.Fatalf("init: %v", err)
	}
	// Switch to the SMS-alert push service when --alert-phone is set
	// (free-tier builds without APNs entitlement).
	if *alertPhone != "" {
		ctrl.SetPushService(&push.SMSAlertService{
			AlertPhone: *alertPhone,
			SendSMS:    ctrl.SendSMSViaModem,
		})
		log.Printf("push: SMS-alert mode, incoming calls will alert %s by SMS", *alertPhone)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		if err := ctrl.Run(ctx); err != nil {
			log.Printf("gateway: %v", err)
		}
	}()
	go func() {
		log.Printf("api: listening on %s", *apiAddr)
		// Timeouts matter here: a handler that waits on the modem must not be
		// able to pin a connection (or the server) forever.
		srv := &http.Server{
			Addr:              *apiAddr,
			Handler:           api.NewHandler(ctrl, *apiToken),
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       30 * time.Second,
			WriteTimeout:      120 * time.Second,
			IdleTimeout:       60 * time.Second,
		}
		if err := srv.ListenAndServe(); err != nil {
			log.Printf("api: %v", err)
		}
	}()

	// Tiny UDP echo service: the phone's media socket can report a successful
	// send while nothing arrives (a tunnel or a wrong interface swallows it).
	// An echo round trip from the phone settles "can this phone send UDP to us
	// at all" without another phone call.
	go echoUDP(":40099")

	log.Printf("gatewayd: modem=%s sip=%s user=%s (fake-audio=%v)",
		*modemFlag, *sipAddr, *user, *loopback)
	log.Printf("gatewayd: point the iPhone app at this machine's IP, SIP %s, user %s", *sipAddr, *user)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Println("gatewayd: shutting down…")
	cancel()
	time.Sleep(500 * time.Millisecond)
}
