package push

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// SMSAlertService wakes the user via a plain SMS when the app is not running
// (free Apple ID build: no APNs, so a VoIP push cannot be sent).
//
// It routes the alert through the same cellular modem that is receiving the
// call — the modem's SendSMS method must be injected at construction time.
// The gateway also extends the INVITE retry window to ~60s so the user has
// time to open the app after reading the SMS.
type SMSAlertService struct {
	// AlertPhone is the user's own phone number that receives the SMS alert.
	AlertPhone string
	// SendSMS sends a text via the AT modem.  Signature matches modem.Driver.SendSMS.
	SendSMS func(ctx context.Context, to, text string) error

	mu          sync.Mutex
	lastAlertAt time.Time
}

const minAlertInterval = 30 * time.Second

// SendVoIP sends an SMS alert when no VoIP push certificate is available.
// The text mirrors what a missed-call notification would say.
func (s *SMSAlertService) SendVoIP(ctx context.Context, deviceToken, callerName, callerURI string) error {
	if s.AlertPhone == "" || s.SendSMS == nil {
		return nil
	}
	s.mu.Lock()
	if time.Since(s.lastAlertAt) < minAlertInterval {
		s.mu.Unlock()
		log.Printf("sms-alert: skipped (too soon, last=%s)", s.lastAlertAt.Format("15:04:05"))
		return nil
	}
	s.lastAlertAt = time.Now()
	s.mu.Unlock()

	caller := callerName
	if caller == "" {
		// Extract number from sip: URI if present
		if strings.HasPrefix(caller, "sip:") {
			caller = strings.TrimPrefix(caller, "sip:")
			if at := strings.Index(caller, "@"); at >= 0 {
				caller = caller[:at]
			}
		}
	}
	text := fmt.Sprintf("[CellBridge] 来电：%s，请打开 App 接听", caller)
	log.Printf("sms-alert: sending to %s: %s", s.AlertPhone, text)
	if err := s.SendSMS(ctx, s.AlertPhone, text); err != nil {
		log.Printf("sms-alert: send failed: %v", err)
		return err
	}
	return nil
}

// SendMessage delivers an SMS notification (used when the app may be closed).
// We forward the body via SIP MESSAGE already in the controller; this is only
// called when push would normally be used — for the free-tier case we just log.
func (s *SMSAlertService) SendMessage(ctx context.Context, deviceToken, from, body string) error {
	// The controller already delivers via SIP MESSAGE; nothing more needed here.
	return nil
}

// SendMissedCall sends an SMS alert for a missed call.
func (s *SMSAlertService) SendMissedCall(ctx context.Context, deviceToken, callerName string) error {
	if s.AlertPhone == "" || s.SendSMS == nil {
		return nil
	}
	text := fmt.Sprintf("[CellBridge] 未接来电：%s", callerName)
	log.Printf("sms-alert: missed call, sending to %s", s.AlertPhone)
	if err := s.SendSMS(ctx, s.AlertPhone, text); err != nil {
		log.Printf("sms-alert: missed send failed: %v", err)
		return err
	}
	return nil
}
