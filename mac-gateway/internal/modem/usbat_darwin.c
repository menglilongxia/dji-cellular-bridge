#include "usbat_darwin.h"

#include <IOKit/IOKitLib.h>
#include <IOKit/IOCFPlugIn.h>
#include <IOKit/usb/IOUSBLib.h>
#include <IOKit/usb/USBSpec.h>
#include <CoreFoundation/CoreFoundation.h>
#include <stdio.h>
#include <string.h>

#define USBAT_IF_AT  3 /* AT command / response */
#define USBAT_IF_URC 2 /* unsolicited result codes */

/* kIOReturnNotResponding-like "no data for now" results we treat as a
 * normal read timeout rather than a hard failure. */
static int usbat_is_timeout(IOReturn kr) {
    return kr == kIOReturnTimeout || kr == (IOReturn)0xe0004051 ||
           kr == (IOReturn)0xe00002d6 /* kIOReturnNotResponding */;
}

struct usbat_handle {
    IOUSBDeviceInterface **dev;
    IOUSBInterfaceInterface **atIf;
    IOUSBInterfaceInterface **urcIf;
    IOUSBInterfaceInterface **adbIf;
    UInt8 atOutPipe;
    UInt8 atInPipe;
    UInt8 urcInPipe;
    UInt8 adbOutPipe;
    UInt8 adbInPipe;
};

static char g_last_error[256];

static int read_pipe(IOUSBInterfaceInterface **iface, UInt8 pipe, void *buf, uint32_t len,
                     uint32_t timeout_ms, uint32_t *out_len);

static void seterr(const char *msg, IOReturn kr) {
    snprintf(g_last_error, sizeof(g_last_error), "%s (0x%08x)", msg, (unsigned)kr);
}

const char *usbat_last_error(void) { return g_last_error; }

/* Opens one interface of the device and picks the bulk in/out pipes.
 * ifNum < 0 ignores the interface number; cls/sub/proto < 0 are "don't care". */
static IOUSBInterfaceInterface **open_interface_match(IOUSBDeviceInterface **dev, int ifNum,
                                                      int cls, int sub, int proto,
                                                      UInt8 *outPipe, UInt8 *inPipe) {
    IOUSBFindInterfaceRequest req;
    req.bInterfaceClass = cls < 0 ? kIOUSBFindInterfaceDontCare : (UInt16)cls;
    req.bInterfaceSubClass = sub < 0 ? kIOUSBFindInterfaceDontCare : (UInt16)sub;
    req.bInterfaceProtocol = proto < 0 ? kIOUSBFindInterfaceDontCare : (UInt16)proto;
    req.bAlternateSetting = kIOUSBFindInterfaceDontCare;

    io_iterator_t iter = 0;
    IOReturn kr = (*dev)->CreateInterfaceIterator(dev, &req, &iter);
    if (kr != kIOReturnSuccess || !iter) {
        seterr("CreateInterfaceIterator", kr);
        return NULL;
    }

    IOUSBInterfaceInterface **found = NULL;
    io_service_t service;
    while ((service = IOIteratorNext(iter))) {
        IOCFPlugInInterface **plug = NULL;
        SInt32 score = 0;
        kr = IOCreatePlugInInterfaceForService(service, kIOUSBInterfaceUserClientTypeID,
                                               kIOCFPlugInInterfaceID, &plug, &score);
        IOObjectRelease(service);
        if (kr != kIOReturnSuccess || !plug) continue;

        IOUSBInterfaceInterface **cand = NULL;
        (*plug)->QueryInterface(plug, CFUUIDGetUUIDBytes(kIOUSBInterfaceInterfaceID),
                                (LPVOID *)&cand);
        (*plug)->Release(plug);
        if (!cand) continue;

        UInt8 num = 255;
        (*cand)->GetInterfaceNumber(cand, &num);
        if (ifNum >= 0 && num != (UInt8)ifNum) {
            (*cand)->Release(cand);
            continue;
        }

        kr = (*cand)->USBInterfaceOpen(cand);
        if (kr != kIOReturnSuccess) {
            seterr("USBInterfaceOpen", kr);
            (*cand)->Release(cand);
            break;
        }
        (*cand)->SetAlternateInterface(cand, 0);

        UInt8 nEP = 0;
        (*cand)->GetNumEndpoints(cand, &nEP);
        for (UInt8 ref = 1; ref <= nEP; ref++) {
            UInt8 dir = 0, num2 = 0, type = 0, interval = 0;
            UInt16 maxp = 0;
            if ((*cand)->GetPipeProperties(cand, ref, &dir, &num2, &type, &maxp, &interval) !=
                kIOReturnSuccess)
                continue;
            if (type != kUSBBulk) continue;
            if (dir == kUSBOut && outPipe && !*outPipe) {
                *outPipe = ref;
            } else if (dir == kUSBIn && inPipe && !*inPipe) {
                *inPipe = ref;
            }
        }
        // Clearing stalls is what makes the first write reliable after the
        // module has been enumerated with an idle endpoint.
        for (UInt8 ref = 1; ref <= nEP; ref++) (*cand)->ClearPipeStallBothEnds(cand, ref);

        found = cand;
        break;
    }
    IOObjectRelease(iter);
    return found;
}

/* Opens one interface of the device and picks the bulk in/out pipes. */
static IOUSBInterfaceInterface **open_interface(IOUSBDeviceInterface **dev, UInt8 ifNum,
                                                UInt8 *outPipe, UInt8 *inPipe) {
    return open_interface_match(dev, (int)ifNum, -1, -1, -1, outPipe, inPipe);
}

usbat_handle *usbat_open(uint16_t vid) {
    g_last_error[0] = '\0';

    CFMutableDictionaryRef match = IOServiceMatching("IOUSBHostDevice");
    if (!match) {
        snprintf(g_last_error, sizeof(g_last_error), "IOServiceMatching failed");
        return NULL;
    }
    io_iterator_t iter = 0;
    IOReturn kr = IOServiceGetMatchingServices(kIOMainPortDefault, match, &iter);
    if (kr != kIOReturnSuccess) {
        seterr("IOServiceGetMatchingServices", kr);
        return NULL;
    }

    io_service_t device;
    while ((device = IOIteratorNext(iter))) {
        IOCFPlugInInterface **plug = NULL;
        SInt32 score = 0;
        kr = IOCreatePlugInInterfaceForService(device, kIOUSBDeviceUserClientTypeID,
                                               kIOCFPlugInInterfaceID, &plug, &score);
        IOObjectRelease(device);
        if (kr != kIOReturnSuccess || !plug) continue;

        IOUSBDeviceInterface **dev = NULL;
        (*plug)->QueryInterface(plug, CFUUIDGetUUIDBytes(kIOUSBDeviceInterfaceID),
                                (LPVOID *)&dev);
        (*plug)->Release(plug);
        if (!dev) continue;

        UInt16 v = 0;
        (*dev)->GetDeviceVendor(dev, &v);
        if (v != vid) {
            (*dev)->Release(dev);
            continue;
        }

        kr = (*dev)->USBDeviceOpen(dev);
        if (kr != kIOReturnSuccess) {
            seterr("USBDeviceOpen", kr);
            (*dev)->Release(dev);
            break;
        }

        usbat_handle *h = calloc(1, sizeof(*h));
        if (!h) {
            snprintf(g_last_error, sizeof(g_last_error), "out of memory");
            (*dev)->Release(dev);
            break;
        }
        h->dev = dev;
        h->atIf = open_interface(dev, USBAT_IF_AT, &h->atOutPipe, &h->atInPipe);
        h->urcIf = open_interface(dev, USBAT_IF_URC, NULL, &h->urcInPipe);

        IOObjectRelease(iter);
        if (!h->atIf) {
            usbat_close(h);
            return NULL;
        }
        return h;
    }
    IOObjectRelease(iter);

    if (g_last_error[0] == '\0')
        snprintf(g_last_error, sizeof(g_last_error), "no USB device with vid 0x%04x", vid);
    return NULL;
}

void usbat_close(usbat_handle *h) {
    if (!h) return;
    if (h->adbIf) {
        (*h->adbIf)->USBInterfaceClose(h->adbIf);
        (*h->adbIf)->Release(h->adbIf);
        h->adbIf = NULL;
    }
    if (h->urcIf) {
        (*h->urcIf)->USBInterfaceClose(h->urcIf);
        (*h->urcIf)->Release(h->urcIf);
    }
    if (h->atIf) {
        (*h->atIf)->USBInterfaceClose(h->atIf);
        (*h->atIf)->Release(h->atIf);
    }
    if (h->dev) {
        (*h->dev)->USBDeviceClose(h->dev);
        (*h->dev)->Release(h->dev);
    }
    free(h);
}

/* --- ADB interface ------------------------------------------------------- */

int usbat_open_adb(usbat_handle *h) {
    if (!h || !h->dev) return -1;
    if (h->adbIf) return 0;
    /* The module's ADB function is a vendor-specific interface with
     * subclass 0x42 / protocol 0x01 and two bulk pipes. */
    h->adbIf = open_interface_match(h->dev, -1, 0xff, 0x42, 0x01, &h->adbOutPipe, &h->adbInPipe);
    if (!h->adbIf) {
        if (g_last_error[0] == '\0') {
            snprintf(g_last_error, sizeof(g_last_error), "no ADB interface (0xff/0x42/0x01)");
        }
        return -1;
    }
    if (!h->adbInPipe || !h->adbOutPipe) {
        seterr("ADB interface has no bulk in/out pair", kIOReturnNotFound);
        (*h->adbIf)->USBInterfaceClose(h->adbIf);
        (*h->adbIf)->Release(h->adbIf);
        h->adbIf = NULL;
        return -1;
    }
    return 0;
}

int usbat_has_adb(usbat_handle *h) { return (h && h->adbIf) ? 1 : 0; }

int usbat_adb_write(usbat_handle *h, const void *buf, uint32_t len) {
    if (!h || !h->adbIf) return -1;
    IOReturn kr = (*h->adbIf)->WritePipeTO(h->adbIf, h->adbOutPipe, (void *)buf, len, 2000, 2000);
    if (kr != kIOReturnSuccess) {
        seterr("ADB WritePipeTO", kr);
        return -1;
    }
    return 0;
}

int usbat_adb_read(usbat_handle *h, void *buf, uint32_t len, uint32_t timeout_ms,
                   uint32_t *out_len) {
    if (!h || !h->adbIf) return -1;
    return read_pipe(h->adbIf, h->adbInPipe, buf, len, timeout_ms, out_len);
}

int usbat_reset(usbat_handle *h, int reenumerate) {
    if (!h || !h->dev) return -1;
    IOReturn kr;
    if (reenumerate) {
        kr = (*h->dev)->USBDeviceReEnumerate(h->dev, 0);
        if (kr != kIOReturnSuccess) {
            seterr("USBDeviceReEnumerate", kr);
            return -1;
        }
        return 0;
    }
    kr = (*h->dev)->ResetDevice(h->dev);
    if (kr != kIOReturnSuccess) {
        seterr("ResetDevice", kr);
        return -1;
    }
    return 0;
}

int usbat_write(usbat_handle *h, const void *buf, uint32_t len) {
    if (!h || !h->atIf) return -1;
    IOReturn kr = (*h->atIf)->WritePipeTO(h->atIf, h->atOutPipe, (void *)buf, len, 1000, 1000);
    if (kr != kIOReturnSuccess) {
        seterr("WritePipeTO", kr);
        return -1;
    }
    return 0;
}

static int read_pipe(IOUSBInterfaceInterface **iface, UInt8 pipe, void *buf, uint32_t len,
                     uint32_t timeout_ms, uint32_t *out_len) {
    if (!iface || !pipe) return -1;
    UInt32 size = len;
    IOReturn kr = (*iface)->ReadPipeTO(iface, pipe, buf, &size, timeout_ms, timeout_ms);
    if (kr == kIOReturnSuccess) {
        *out_len = size;
        return 0;
    }
    if (usbat_is_timeout(kr)) {
        *out_len = 0;
        return 0;
    }
    seterr("ReadPipeTO", kr);
    return -1;
}

int usbat_read_at(usbat_handle *h, void *buf, uint32_t len, uint32_t timeout_ms,
                  uint32_t *out_len) {
    if (!h) return -1;
    return read_pipe(h->atIf, h->atInPipe, buf, len, timeout_ms, out_len);
}

int usbat_read_urc(usbat_handle *h, void *buf, uint32_t len, uint32_t timeout_ms,
                   uint32_t *out_len) {
    if (!h) return -1;
    return read_pipe(h->urcIf, h->urcInPipe, buf, len, timeout_ms, out_len);
}
