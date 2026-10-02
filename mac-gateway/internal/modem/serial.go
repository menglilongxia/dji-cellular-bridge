package modem

import (
	"context"
	"errors"
)

// SerialModem is the real-hardware driver: DJI QDC507 over a USB serial
// AT port (e.g. /dev/tty.usbmodem* on macOS, /dev/ttyUSB2 on Linux).
//
// NOT IMPLEMENTED YET — hardware integration happens after the
// server + app are done (see PROJECT.md milestone 1, the USB spike).
// The interface is fixed now so the rest of the gateway can be built
// and tested against FakeModem.
type SerialModem struct {
	port string
}

var _ Driver = (*SerialModem)(nil)

var errNotImplemented = errors.New("serial modem: not implemented yet (hardware phase)")

func NewSerialModem(port string) *SerialModem { return &SerialModem{port: port} }

func (m *SerialModem) Name() string { return "serial:" + m.port }

func (m *SerialModem) Events(ctx context.Context) (<-chan Event, error) {
	return nil, errNotImplemented
}
func (m *SerialModem) Close() error { return nil }

func (m *SerialModem) EnableCallerID(ctx context.Context) error { return errNotImplemented }
func (m *SerialModem) Dial(ctx context.Context, number string) error {
	return errNotImplemented
}
func (m *SerialModem) Answer(ctx context.Context) error { return errNotImplemented }
func (m *SerialModem) Hangup(ctx context.Context) error { return errNotImplemented }
func (m *SerialModem) SendSMS(ctx context.Context, to, text string) error {
	return errNotImplemented
}
func (m *SerialModem) ReadSMS(ctx context.Context, index int) (string, string, error) {
	return "", "", errNotImplemented
}
func (m *SerialModem) DeleteSMS(ctx context.Context, index int) error {
	return errNotImplemented
}
func (m *SerialModem) Signal(ctx context.Context) (int, error) { return 99, errNotImplemented }
func (m *SerialModem) AudioDevice() string                     { return "" }
