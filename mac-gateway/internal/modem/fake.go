package modem

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// FakeOptions configures scripted demo events. Zero value = quiet modem.
type FakeOptions struct {
	IncomingCallAfter time.Duration
	IncomingCaller    string
	IncomingSMSAfter  time.Duration
	IncomingSMSFrom   string
	IncomingSMSText   string
	// DialConnectAfter simulates the remote party answering an outgoing call.
	DialConnectAfter time.Duration
}

type storedSMS struct{ from, text string }

// FakeModem simulates a cellular module without hardware.
// Scripted events fire on timers; tests / the HTTP API can also trigger
// events on demand via TriggerIncomingCall / TriggerIncomingSMS.
type FakeModem struct {
	opts FakeOptions

	mu     sync.Mutex
	trig   chan Event
	inbox  []storedSMS
	outbox []storedSMS
	rssi   int
}

var _ Driver = (*FakeModem)(nil)

func NewFakeModem(opts FakeOptions) *FakeModem {
	if opts.DialConnectAfter == 0 {
		opts.DialConnectAfter = 1500 * time.Millisecond
	}
	if opts.IncomingCaller == "" {
		opts.IncomingCaller = "13800138000"
	}
	if opts.IncomingSMSFrom == "" {
		opts.IncomingSMSFrom = "10086"
	}
	if opts.IncomingSMSText == "" {
		opts.IncomingSMSText = "FakeModem: 这是一条模拟短信"
	}
	return &FakeModem{opts: opts, trig: make(chan Event, 64), rssi: 20}
}

func (f *FakeModem) Name() string { return "fake" }

func (f *FakeModem) Events(ctx context.Context) (<-chan Event, error) {
	out := make(chan Event, 64)
	go func() {
		defer close(out)
		var timers []*time.Timer
		arm := func(d time.Duration, ev Event) {
			if d <= 0 {
				return
			}
			t := time.AfterFunc(d, func() {
				ev.At = time.Now()
				select {
				case out <- ev:
				default:
				}
			})
			timers = append(timers, t)
		}
		defer func() {
			for _, t := range timers {
				t.Stop()
			}
		}()

		arm(f.opts.IncomingCallAfter, Event{Type: EventIncomingCall, Number: f.opts.IncomingCaller})
		if f.opts.IncomingSMSAfter > 0 {
			f.mu.Lock()
			f.inbox = append(f.inbox, storedSMS{from: f.opts.IncomingSMSFrom, text: f.opts.IncomingSMSText})
			idx := len(f.inbox)
			f.mu.Unlock()
			arm(f.opts.IncomingSMSAfter, Event{Type: EventIncomingSMS, Number: f.opts.IncomingSMSFrom, Index: idx})
		}

		for {
			select {
			case <-ctx.Done():
				return
			case ev := <-f.trig:
				ev.At = time.Now()
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

func (f *FakeModem) emit(ev Event) {
	ev.At = time.Now()
	select {
	case f.trig <- ev:
	default:
	}
}

func (f *FakeModem) Close() error { return nil }

func (f *FakeModem) EnableCallerID(ctx context.Context) error { return nil }

func (f *FakeModem) Dial(ctx context.Context, number string) error {
	go func() {
		select {
		case <-time.After(f.opts.DialConnectAfter):
			f.emit(Event{Type: EventCallConnected, Number: number})
		case <-ctx.Done():
		}
	}()
	return nil
}

func (f *FakeModem) Answer(ctx context.Context) error {
	f.emit(Event{Type: EventCallConnected})
	return nil
}

func (f *FakeModem) Hangup(ctx context.Context) error {
	f.emit(Event{Type: EventCallEnded})
	return nil
}

func (f *FakeModem) SendSMS(ctx context.Context, to, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.outbox = append(f.outbox, storedSMS{from: to, text: text})
	return nil
}

func (f *FakeModem) ReadSMS(ctx context.Context, index int) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if index < 1 || index > len(f.inbox) {
		return "", "", fmt.Errorf("fake modem: no SMS at index %d", index)
	}
	s := f.inbox[index-1]
	return s.from, s.text, nil
}

func (f *FakeModem) DeleteSMS(ctx context.Context, index int) error { return nil }

func (f *FakeModem) Signal(ctx context.Context) (int, error) { return f.rssi, nil }

func (f *FakeModem) AudioDevice() string { return "" }

// TriggerIncomingCall injects an incoming-call event (tests / HTTP API).
func (f *FakeModem) TriggerIncomingCall(number string) {
	if number == "" {
		number = f.opts.IncomingCaller
	}
	f.emit(Event{Type: EventIncomingCall, Number: number})
}

// TriggerIncomingSMS injects an incoming-SMS event (tests / HTTP API).
func (f *FakeModem) TriggerIncomingSMS(from, text string) {
	if from == "" {
		from = f.opts.IncomingSMSFrom
	}
	if text == "" {
		text = f.opts.IncomingSMSText
	}
	f.mu.Lock()
	f.inbox = append(f.inbox, storedSMS{from: from, text: text})
	idx := len(f.inbox)
	f.mu.Unlock()
	f.emit(Event{Type: EventIncomingSMS, Number: from, Index: idx})
}

// Outbox returns sent SMS (for tests).
func (f *FakeModem) Outbox() []storedSMS {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]storedSMS, len(f.outbox))
	copy(cp, f.outbox)
	return cp
}
