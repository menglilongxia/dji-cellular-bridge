//go:build !(darwin && cgo)

package modem

import (
	"context"
	"errors"

	"github.com/dji-cellular-bridge/mac-gateway/internal/adb"
)

// USBATModem is unavailable when built without cgo or off macOS.
type USBATModem struct{}

var errUSBATModemUnavailable = errors.New(
	"qdc507: real module driver needs darwin + cgo (build on macOS)")

func NewUSBATModem() (*USBATModem, error) { return nil, errUSBATModemUnavailable }

// The stub still satisfies Driver so gateway code compiles everywhere.
var _ Driver = (*USBATModem)(nil)

func (m *USBATModem) Name() string        { return "qdc507-unavailable" }
func (m *USBATModem) Close() error        { return nil }
func (m *USBATModem) AudioDevice() string { return "" }
func (m *USBATModem) Events(ctx context.Context) (<-chan Event, error) {
	return nil, errUSBATModemUnavailable
}
func (m *USBATModem) EnableCallerID(ctx context.Context) error { return errUSBATModemUnavailable }
func (m *USBATModem) Dial(ctx context.Context, number string) error {
	return errUSBATModemUnavailable
}
func (m *USBATModem) Answer(ctx context.Context) error { return errUSBATModemUnavailable }
func (m *USBATModem) Hangup(ctx context.Context) error { return errUSBATModemUnavailable }
func (m *USBATModem) SendSMS(ctx context.Context, to, text string) error {
	return errUSBATModemUnavailable
}
func (m *USBATModem) ReadSMS(ctx context.Context, index int) (string, string, error) {
	return "", "", errUSBATModemUnavailable
}
func (m *USBATModem) DeleteSMS(ctx context.Context, index int) error {
	return errUSBATModemUnavailable
}
func (m *USBATModem) Signal(ctx context.Context) (int, error) {
	return 99, errUSBATModemUnavailable
}

// ADBClient is unavailable without darwin + cgo.
func (m *USBATModem) ADBClient() *adb.Client { return nil }

// RecoverADB is unavailable without darwin + cgo.
func (m *USBATModem) RecoverADB(ctx context.Context) error { return errUSBATModemUnavailable }
