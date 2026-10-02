//go:build !(darwin && cgo)

package modem

import "errors"

// QDC507VendorIDs lists the vendor ids of the DJI QDC507 module.
const (
	QDC507VendorIDEnabled = 0x2c7c
	QDC507VendorIDFactory = 0x2ca3
)

var QDC507VendorIDs = []uint16{QDC507VendorIDEnabled, QDC507VendorIDFactory}

// USBAT is unavailable when the binary is built without cgo (or off macOS).
// The fake-modem baseline keeps working in that configuration.
type USBAT struct{}

var errUSBATUnavailable = errors.New(
	"usbat: real USB AT transport needs darwin + cgo (build on macOS with CommandLineTools)")

func OpenUSBAT(vid uint16) (*USBAT, error) { return nil, errUSBATUnavailable }

func (u *USBAT) Write(p []byte) error                        { return errUSBATUnavailable }
func (u *USBAT) ReadAT(p []byte, timeoutMs int) (int, error) { return 0, errUSBATUnavailable }
func (u *USBAT) ReadURC(p []byte, timeoutMs int) (int, error) {
	return 0, errUSBATUnavailable
}
func (u *USBAT) Close() error { return nil }

func (u *USBAT) OpenADB() error                               { return errUSBATUnavailable }
func (u *USBAT) HasADB() bool                                 { return false }
func (u *USBAT) ADBWrite(p []byte) error                      { return errUSBATUnavailable }
func (u *USBAT) ADBRead(p []byte, timeoutMs int) (int, error) { return 0, errUSBATUnavailable }
func (u *USBAT) Reset(reEnumerate bool) error                 { return errUSBATUnavailable }
