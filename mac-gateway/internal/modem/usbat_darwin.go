//go:build darwin && cgo

package modem

/*
#cgo CFLAGS: -I${SRCDIR}
#cgo LDFLAGS: -framework IOKit -framework CoreFoundation
#include <stdlib.h>
#include "usbat_darwin.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"unsafe"
)

// QDC507VendorIDs lists the vendor ids the DJI QDC507 module is known to
// enumerate with. There is no /dev/tty.* node either way — the module only has
// vendor-specific (class 255) interfaces, which macOS binds no driver to.
//
//	0x2c7c — after `AT+QCFG="usbcfg",0x2C7C,0x0125,1,1,1,1,1,1,1`
//	         (AT + NMEA + DIAG + Modem + ECM + ADB + UAC, the composition
//	         CellBridge documents). This is the config we want: it is what
//	         exposes the USB Audio interface used for the voice path.
//	0x2ca3 — factory default (Baiwang), no audio, no adb.
const (
	QDC507VendorIDEnabled = 0x2c7c
	QDC507VendorIDFactory = 0x2ca3
)

// QDC507VendorIDs is tried in order when opening the module.
var QDC507VendorIDs = []uint16{QDC507VendorIDEnabled, QDC507VendorIDFactory}

// USBAT is a user-space USB transport for the module's AT + URC interfaces.
// It is not safe for concurrent use; the gateway serialises AT traffic.
type USBAT struct {
	handle *C.usbat_handle
}

// OpenUSBAT claims the AT and URC interfaces of the first device with vid.
// Requires darwin + cgo; see usbat_other.go for the stub used elsewhere.
func OpenUSBAT(vid uint16) (*USBAT, error) {
	h := C.usbat_open(C.uint16_t(vid))
	if h == nil {
		return nil, fmt.Errorf("usbat: open failed: %s", C.GoString(C.usbat_last_error()))
	}
	return &USBAT{handle: h}, nil
}

// Write sends raw bytes to the AT interface.
func (u *USBAT) Write(p []byte) error {
	if len(p) == 0 {
		return nil
	}
	rc := C.usbat_write(u.handle, unsafe.Pointer(&p[0]), C.uint32_t(len(p)))
	if rc != 0 {
		return fmt.Errorf("usbat: write failed: %s", C.GoString(C.usbat_last_error()))
	}
	return nil
}

// ReadAT reads bytes from the AT response interface. A timeout yields (0, nil).
func (u *USBAT) ReadAT(p []byte, timeoutMs int) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	var n C.uint32_t
	rc := C.usbat_read_at(u.handle, unsafe.Pointer(&p[0]), C.uint32_t(len(p)),
		C.uint32_t(timeoutMs), &n)
	if rc != 0 {
		return 0, fmt.Errorf("usbat: read failed: %s", C.GoString(C.usbat_last_error()))
	}
	return int(n), nil
}

// ReadURC reads bytes from the unsolicited-result-code interface.
func (u *USBAT) ReadURC(p []byte, timeoutMs int) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	var n C.uint32_t
	rc := C.usbat_read_urc(u.handle, unsafe.Pointer(&p[0]), C.uint32_t(len(p)),
		C.uint32_t(timeoutMs), &n)
	if rc != 0 {
		return 0, fmt.Errorf("usbat: read failed: %s", C.GoString(C.usbat_last_error()))
	}
	return int(n), nil
}

// Close releases the USB interfaces.
func (u *USBAT) Close() error {
	if u.handle != nil {
		C.usbat_close(u.handle)
		u.handle = nil
	}
	return nil
}

// --- ADB interface ---------------------------------------------------------

// OpenADB claims the module's ADB interface on the device this transport
// already owns. Doing it here — rather than from a second process — is what
// makes per-call provisioning reliable: the gateway is the only owner of the
// USB device, so nothing can seize the pipes out from under the AT channel.
func (u *USBAT) OpenADB() error {
	if u.handle == nil {
		return errors.New("usbat: transport is closed")
	}
	if C.usbat_open_adb(u.handle) != 0 {
		return fmt.Errorf("usbat: claim ADB interface: %s", C.GoString(C.usbat_last_error()))
	}
	return nil
}

// HasADB reports whether the ADB interface is claimed.
func (u *USBAT) HasADB() bool {
	return u.handle != nil && C.usbat_has_adb(u.handle) == 1
}

// ADBWrite sends one USB bulk transfer on the ADB OUT pipe. Callers must not
// merge an ADB header with its payload — see internal/adb.
func (u *USBAT) ADBWrite(p []byte) error {
	if u.handle == nil {
		return errors.New("usbat: transport is closed")
	}
	if len(p) == 0 {
		return nil
	}
	if C.usbat_adb_write(u.handle, unsafe.Pointer(&p[0]), C.uint32_t(len(p))) != 0 {
		return fmt.Errorf("usbat: adb write: %s", C.GoString(C.usbat_last_error()))
	}
	return nil
}

// ADBRead reads one USB bulk transfer from the ADB IN pipe. A timeout yields
// (0, nil).
func (u *USBAT) ADBRead(p []byte, timeoutMs int) (int, error) {
	if u.handle == nil {
		return 0, errors.New("usbat: transport is closed")
	}
	if len(p) == 0 {
		return 0, nil
	}
	var n C.uint32_t
	if C.usbat_adb_read(u.handle, unsafe.Pointer(&p[0]), C.uint32_t(len(p)),
		C.uint32_t(timeoutMs), &n) != 0 {
		return 0, fmt.Errorf("usbat: adb read: %s", C.GoString(C.usbat_last_error()))
	}
	return int(n), nil
}

// Reset resets the USB device. With reEnumerate the device leaves the bus and
// comes back (every interface handle in this process becomes invalid, so the
// caller must re-open); without it the device is reset in place. This is the
// only way to revive a wedged adbd, which otherwise refuses every service open
// until the module is power cycled.
func (u *USBAT) Reset(reEnumerate bool) error {
	if u.handle == nil {
		return errors.New("usbat: transport is closed")
	}
	flag := C.int(0)
	if reEnumerate {
		flag = 1
	}
	if C.usbat_reset(u.handle, flag) != 0 {
		return fmt.Errorf("usbat: reset: %s", C.GoString(C.usbat_last_error()))
	}
	return nil
}
