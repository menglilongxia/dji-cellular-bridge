package push

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestSMSAlertService_SendVoIP(t *testing.T) {
	var sent atomic.Int32
	var gotTo, gotText string

	svc := &SMSAlertService{
		AlertPhone: "+8613800138000",
		SendSMS: func(ctx context.Context, to, text string) error {
			sent.Add(1)
			gotTo = to
			gotText = text
			return nil
		},
	}

	err := svc.SendVoIP(context.Background(), "", "13912345678", "sip:13912345678@gw")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sent.Load() != 1 {
		t.Fatalf("expected 1 SMS, got %d", sent.Load())
	}
	if gotTo != "+8613800138000" {
		t.Errorf("wrong recipient: %s", gotTo)
	}
	if gotText == "" {
		t.Error("empty SMS body")
	}
	t.Logf("SMS body: %s", gotText)
}

func TestSMSAlertService_RateLimit(t *testing.T) {
	var sent atomic.Int32
	svc := &SMSAlertService{
		AlertPhone: "+8613800138000",
		SendSMS:    func(_ context.Context, _, _ string) error { sent.Add(1); return nil },
		// inject a very recent lastAlertAt to trigger rate limiting
	}
	svc.lastAlertAt = time.Now() // already "just sent"

	_ = svc.SendVoIP(context.Background(), "", "foo", "")
	if sent.Load() != 0 {
		t.Errorf("expected rate-limiting to suppress send, got %d", sent.Load())
	}
}

func TestSMSAlertService_MissedCall(t *testing.T) {
	var sent atomic.Int32
	svc := &SMSAlertService{
		AlertPhone: "+8613800138000",
		SendSMS:    func(_ context.Context, _, _ string) error { sent.Add(1); return nil },
	}
	_ = svc.SendMissedCall(context.Background(), "", "13912345678")
	if sent.Load() != 1 {
		t.Fatalf("expected 1 SMS for missed call, got %d", sent.Load())
	}
}

func TestSMSAlertService_NoAlertPhoneIsNoop(t *testing.T) {
	svc := &SMSAlertService{
		SendSMS: func(_ context.Context, _, _ string) error {
			panic("should not be called")
		},
	}
	if err := svc.SendVoIP(context.Background(), "", "foo", ""); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}
