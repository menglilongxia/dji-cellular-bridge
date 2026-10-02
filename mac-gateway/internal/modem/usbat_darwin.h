#ifndef CELLBRIDGE_USBAT_DARWIN_H
#define CELLBRIDGE_USBAT_DARWIN_H

#include <stdint.h>

/*
 * Minimal user-space USB AT transport for the DJI QDC507 module on macOS.
 *
 * The module enumerates as a CDC composite device whose AT functions are
 * vendor-specific (class 255), so macOS binds no driver and no /dev/tty.*
 * appears. We talk to the interfaces directly through IOUSBLib.
 *
 * Interfaces used (verified against QDC507 firmware QDC507GLEFM21):
 *   AT  commands : interface 3, bulk OUT 0x04 / bulk IN 0x86
 *   URCs         : interface 2, bulk IN 0x84
 */

typedef struct usbat_handle usbat_handle;

/* Opens the first USB device with the given vendor id and claims the AT +
 * URC interfaces. Returns NULL on failure (see usbat_last_error). */
usbat_handle *usbat_open(uint16_t vid);

void usbat_close(usbat_handle *h);

/* Claims the ADB interface (class 0xff / subclass 0x42 / protocol 0x01) of an
 * already-opened device. This is the whole point of doing ADB in-process: the
 * gateway is the only owner of the device, so there is no second process to
 * fight over the USB pipes. Returns 0 on success, -1 when the device has no
 * ADB interface or it cannot be claimed. */
int usbat_open_adb(usbat_handle *h);

/* True once usbat_open_adb succeeded. */
int usbat_has_adb(usbat_handle *h);

/* Raw bulk transfer on the ADB pipes. Each call is exactly one USB transfer,
 * which is what ADB-over-USB needs (header and payload must not be merged). */
int usbat_adb_write(usbat_handle *h, const void *buf, uint32_t len);
int usbat_adb_read(usbat_handle *h, void *buf, uint32_t len, uint32_t timeout_ms,
                   uint32_t *out_len);

/* Device-level recovery for a wedged adbd (the module's adbd stops accepting
 * service opens and never comes back by itself).
 * mode 0 = ResetDevice, mode 1 = USBDeviceReEnumerate (the device leaves and
 * re-appears, so the caller must re-open everything afterwards). */
int usbat_reset(usbat_handle *h, int reenumerate);

/* Writes raw bytes to the AT port. Returns 0 on success, negative on error. */
int usbat_write(usbat_handle *h, const void *buf, uint32_t len);

/* Reads AT responses. *out_len is set to the byte count (0 on timeout).
 * Returns 0 on success (including timeout), negative on hard error. */
int usbat_read_at(usbat_handle *h, void *buf, uint32_t len, uint32_t timeout_ms,
                  uint32_t *out_len);

/* Reads unsolicited result codes. Same contract as usbat_read_at. */
int usbat_read_urc(usbat_handle *h, void *buf, uint32_t len, uint32_t timeout_ms,
                   uint32_t *out_len);

/* Human-readable description of the last failure. */
const char *usbat_last_error(void);

#endif /* CELLBRIDGE_USBAT_DARWIN_H */
