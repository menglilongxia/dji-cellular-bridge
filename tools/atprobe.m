// atprobe.m — probe a vendor-specific USB composite device for an AT command port.
// macOS has no driver for class-255 interfaces, so we talk to them from user space
// via IOUSBLib. Usage: atprobe [interfaceNumber] [command]

#import <Foundation/Foundation.h>
#include <IOKit/IOKitLib.h>
#include <IOKit/IOCFPlugIn.h>
#include <IOKit/usb/IOUSBLib.h>
#include <IOKit/usb/USBSpec.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <signal.h>
#include <unistd.h>

static IOUSBDeviceInterface **devIf = NULL;
static IOUSBInterfaceInterface **ifIf = NULL;

static void onAlarm(int sig) {
    (void)sig;
    const char *msg = "!! timeout — a USB call blocked, bailing out\n";
    write(STDOUT_FILENO, msg, strlen(msg));
    _exit(3);
}

static void dumpDescriptors(const IOUSBConfigurationDescriptor *cfg) {
    const UInt8 *p = (const UInt8 *)cfg;
    UInt16 total = cfg->wTotalLength;
    UInt16 i = cfg->bLength;
    int curIface = -1;
    printf("  config: total=%u interfaces=%u\n", total, cfg->bNumInterfaces);
    while (i + 2 <= total) {
        UInt8 len = p[i], type = p[i + 1];
        if (len < 2 || i + len > total) break;
        if (type == kUSBInterfaceDesc) {
            const IOUSBInterfaceDescriptor *d = (const IOUSBInterfaceDescriptor *)(p + i);
            curIface = d->bInterfaceNumber;
            printf("  [IF %d] alt=%d class=%d sub=%d proto=%d endpoints=%d\n",
                   d->bInterfaceNumber, d->bAlternateSetting, d->bInterfaceClass,
                   d->bInterfaceSubClass, d->bInterfaceProtocol, d->bNumEndpoints);
        } else if (type == kUSBEndpointDesc) {
            const IOUSBEndpointDescriptor *e = (const IOUSBEndpointDescriptor *)(p + i);
            UInt8 addr = e->bEndpointAddress;
            const char *dir = (addr & 0x80) ? "IN " : "OUT";
            const char *kind = (e->bmAttributes & 0x03) == kUSBBulk
                                   ? "bulk"
                                   : ((e->bmAttributes & 0x03) == kUSBInterrupt ? "intr" : "ctrl/iso");
            printf("      [IF %d] ep 0x%02x %s %-8s maxpkt=%u interval=%u\n",
                   curIface, addr, dir, kind, e->wMaxPacketSize, e->bInterval);
        }
        i += len;
    }
}

int main(int argc, char **argv) {
    @autoreleasepool {
        setvbuf(stdout, NULL, _IONBF, 0);
        signal(SIGALRM, onAlarm);
        alarm(30);

        int targetIface = (argc > 1) ? atoi(argv[1]) : -1;
        const char *command = (argc > 2) ? argv[2] : "AT\r";

        kern_return_t kr;
        io_iterator_t iter = 0;
        io_service_t device = 0;

        CFMutableDictionaryRef match = IOServiceMatching("IOUSBHostDevice");

        kr = IOServiceGetMatchingServices(kIOMainPortDefault, match, &iter);
        if (kr != KERN_SUCCESS) { printf("match failed: 0x%x\n", kr); return 1; }

        int seen = 0, handled = 0;
        while ((device = IOIteratorNext(iter))) {
            seen++;
            IOCFPlugInInterface **plug = NULL;
            SInt32 score = 0;
            kr = IOCreatePlugInInterfaceForService(device, kIOUSBDeviceUserClientTypeID,
                                                   kIOCFPlugInInterfaceID, &plug, &score);
            if (kr == KERN_SUCCESS && plug) {
                (*plug)->QueryInterface(plug, CFUUIDGetUUIDBytes(kIOUSBDeviceInterfaceID),
                                        (LPVOID *)&devIf);
                (*plug)->Release(plug);
            }
            if (!devIf) { IOObjectRelease(device); continue; }

            UInt16 vendor = 0, product = 0;
            (*devIf)->GetDeviceVendor(devIf, &vendor);
            (*devIf)->GetDeviceProduct(devIf, &product);
            if (targetIface < 0)
                printf("device: VID=0x%04x PID=0x%04x%s\n", vendor, product,
                       vendor == 0x2ca3 ? "   <== target" : "");

            if (vendor != 0x2ca3) {
                (*devIf)->Release(devIf);
                devIf = NULL;
                IOObjectRelease(device);
                continue;
            }
            handled++;
            printf("device: VID=0x%04x PID=0x%04x\n", vendor, product);

            kr = (*devIf)->USBDeviceOpen(devIf);
            printf("USBDeviceOpen -> 0x%x (%s)\n", kr,
                   kr == kIOReturnSuccess ? "ok" : "FAILED");

            UInt8 numConfig = 0;
            (*devIf)->GetNumberOfConfigurations(devIf, &numConfig);
            IOUSBConfigurationDescriptor *cfg = NULL;
            (*devIf)->GetConfigurationDescriptorPtr(devIf, 0, &cfg);
            if (cfg) dumpDescriptors(cfg);

            if (targetIface >= 0 && cfg) {
                IOUSBFindInterfaceRequest req;
                req.bInterfaceClass = kIOUSBFindInterfaceDontCare;
                req.bInterfaceSubClass = kIOUSBFindInterfaceDontCare;
                req.bInterfaceProtocol = kIOUSBFindInterfaceDontCare;
                req.bAlternateSetting = kIOUSBFindInterfaceDontCare;
                io_iterator_t ifIter = 0;
                kr = (*devIf)->CreateInterfaceIterator(devIf, &req, &ifIter);
                if (kr == KERN_SUCCESS && ifIter) {
                    io_service_t ifService;
                    while ((ifService = IOIteratorNext(ifIter))) {
                        IOCFPlugInInterface **ip = NULL;
                        SInt32 sc = 0;
                        kr = IOCreatePlugInInterfaceForService(ifService,
                              kIOUSBInterfaceUserClientTypeID, kIOCFPlugInInterfaceID, &ip, &sc);
                        if (kr != KERN_SUCCESS || !ip) { IOObjectRelease(ifService); continue; }
                        IOUSBInterfaceInterface **tmpIf = NULL;
                        (*ip)->QueryInterface(ip, CFUUIDGetUUIDBytes(kIOUSBInterfaceInterfaceID),
                                              (LPVOID *)&tmpIf);
                        (*ip)->Release(ip);
                        IOObjectRelease(ifService);
                        if (!tmpIf) continue;

                        UInt8 ifNum = 255;
                        (*tmpIf)->GetInterfaceNumber(tmpIf, &ifNum);
                        if (ifNum != targetIface) { (*tmpIf)->Release(tmpIf); continue; }

                        ifIf = tmpIf;
                        break;
                    }
                    IOObjectRelease(ifIter);
                }

                if (ifIf) {
                    kr = (*ifIf)->USBInterfaceOpen(ifIf);
                    printf("\nopen IF %d -> 0x%x (%s)\n", targetIface, kr,
                           kr == kIOReturnSuccess ? "ok" : "FAILED");
                    if (kr == kIOReturnSuccess) {
                        kr = (*ifIf)->SetAlternateInterface(ifIf, 0);
                        printf("  SetAlternateInterface(0) -> 0x%x\n", kr);
                        UInt8 nEP = 0;
                        (*ifIf)->GetNumEndpoints(ifIf, &nEP);
                        UInt8 outPipe = 0, inPipe = 0;
                        for (UInt8 pipeRef = 1; pipeRef <= nEP; pipeRef++) {
                            UInt8 addr = 0, num = 0, dir = 0, type = 0, interval = 0;
                            UInt16 maxp = 0;
                            kr = (*ifIf)->GetPipeProperties(ifIf, pipeRef, &dir, &num, &type,
                                                            &maxp, &interval);
                            if (kr != kIOReturnSuccess) continue;
                            addr = (dir == kUSBIn ? 0x80 : 0x00) | num;
                            printf("  pipe %d: addr=0x%02x %s type=%d maxpkt=%u\n", pipeRef, addr,
                                   dir == kUSBIn ? "IN" : "OUT", type, maxp);
                            if (type == kUSBBulk && dir == kUSBOut && !outPipe) outPipe = pipeRef;
                            if (type == kUSBBulk && dir == kUSBIn && !inPipe) inPipe = pipeRef;
                        }
                        for (UInt8 pipeRef = 1; pipeRef <= nEP; pipeRef++)
                            (*ifIf)->ClearPipeStallBothEnds(ifIf, pipeRef);

                        if (outPipe && inPipe) {
                            UInt32 len = (UInt32)strlen(command);
                            kr = (*ifIf)->WritePipeTO(ifIf, outPipe, (void *)command, len, 1000,
                                                      1000);
                            printf("  write \"%s\" -> 0x%x\n", command, kr);
                            char buf[1024];
                            for (int round = 0; round < 3; round++) {
                                UInt32 size = sizeof(buf) - 1;
                                kr = (*ifIf)->ReadPipeTO(ifIf, inPipe, buf, &size, 1500, 1500);
                                if (kr != kIOReturnSuccess) {
                                    printf("  read -> 0x%x (no data)\n", kr);
                                    break;
                                }
                                buf[size] = 0;
                                printf("  <<< %s", buf);
                                if (size) printf("\n");
                                if (strstr(buf, "OK") || strstr(buf, "ERROR")) break;
                            }
                        } else {
                            printf("  no bulk in/out pair on this interface (out=%u in=%u)\n",
                                   outPipe, inPipe);
                        }
                        (*ifIf)->USBInterfaceClose(ifIf);
                    }
                    (*ifIf)->Release(ifIf);
                    ifIf = NULL;
                } else {
                    printf("\ninterface %d not found\n", targetIface);
                }
            }

            (*devIf)->Release(devIf);
            devIf = NULL;
            IOObjectRelease(device);
        }
        IOObjectRelease(iter);
        printf("scanned %d IOUSBHostDevice service(s); %d matched VID 0x2ca3\n", seen, handled);
    }
    return 0;
}
