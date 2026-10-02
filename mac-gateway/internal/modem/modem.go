// Package modem abstracts the cellular module control plane.
//
// The production driver talks to a DJI QDC507 4G module over a USB serial
// AT port. For development without hardware, FakeModem simulates the same
// event stream (incoming calls, SMS, connect/disconnect).
package modem

import (
	"context"
	"time"
)

// EventType identifies a modem unsolicited event.
type EventType string

const (
	// EventIncomingCall: RING / +CLIP. Number is the caller.
	EventIncomingCall EventType = "incoming-call"
	// EventCallConnected: voice path is active (after Dial or Answer).
	EventCallConnected EventType = "call-connected"
	// EventCallEnded: NO CARRIER / BUSY / NO ANSWER / ATH done.
	EventCallEnded EventType = "call-ended"
	// EventIncomingSMS: +CMTI. Index is the storage index to read.
	EventIncomingSMS EventType = "incoming-sms"
	// EventSignal: signal quality update. RSSI 0-31, 99 = unknown.
	EventSignal EventType = "signal"
)

// Event is one unsolicited modem event.
type Event struct {
	Type   EventType
	Number string // peer number for call / SMS sender
	Index  int    // SMS storage index (EventIncomingSMS)
	RSSI   int    // signal strength (EventSignal)
	At     time.Time
}

// Driver is the cellular module control plane.
type Driver interface {
	Name() string

	// Events streams unsolicited events until ctx is done, then closes the channel.
	Events(ctx context.Context) (<-chan Event, error)
	Close() error

	// Call control.
	EnableCallerID(ctx context.Context) error // AT+CLIP=1
	Dial(ctx context.Context, number string) error
	Answer(ctx context.Context) error
	Hangup(ctx context.Context) error

	// SMS.
	SendSMS(ctx context.Context, to, text string) error
	ReadSMS(ctx context.Context, index int) (from, text string, err error)
	DeleteSMS(ctx context.Context, index int) error

	// Signal returns RSSI 0-31 (99 = unknown).
	Signal(ctx context.Context) (rssi int, err error)

	// AudioDevice returns the OS audio device carrying call audio,
	// e.g. "hw:0,0" on Linux or a CoreAudio UID on macOS.
	// Empty when the driver has no real audio path (fake mode).
	AudioDevice() string
}
