//go:build darwin && cgo && qdc507libusb

// qdc507-push brings the module-side voice runtime onto a DJI QDC507 over
// ADB-over-USB, and starts it.
//
// SUPERSEDED. The gateway now speaks ADB itself (internal/adb) over the USB
// device it already owns; running this tool next to the gateway does not work
// any more, and used to be actively harmful: it seizes the device away from
// the gateway, which breaks the AT channel and wedges the module's adbd.
// Keep it only as the reference implementation of the wire protocol, built on
// demand with `go build -tags qdc507libusb ./cmd/qdc507-push` (needs libusb +
// pkg-config). For day-to-day work use `cmd/qdc507-adb`, which needs neither.
//
// macOS has no adb for this device (its vendor id is not in adb's list), so
// this speaks the ADB wire protocol directly over libusb, the same way the
// reference implementation does.
//
// The critical detail (it cost hours): the 24-byte message header and its
// payload must be written as TWO separate USB bulk transfers. Writing them in
// one transfer is silently ignored by this module's adbd — it simply never
// answers.
//
//	qdc507-push [-state] push   <dir>        # push *.ko / mavo-pcm-bridge.armv7
//	qdc507-push -state shell "<command>"     # run a shell command as root
//	qdc507-push -state start                 # insmod + start the voice route
//	qdc507-push -state stop                  # stop the route
package main

/*
#cgo pkg-config: libusb-1.0
#include <libusb.h>
#include <stdlib.h>
*/
import "C"

import (
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unsafe"
)

const (
	vendorID  = 0x2c7c
	productID = 0x0125

	adbCNXN = 0x4e584e43
	adbAUTH = 0x48545541
	adbOPEN = 0x4e45504f
	adbOKAY = 0x59414b4f
	adbCLSE = 0x45534c43
	adbWRTE = 0x45545257

	syncSTAT = 0x54415453
	syncSEND = 0x444e4553
	syncDATA = 0x41544144
	syncDONE = 0x454e4f44
	syncFAIL = 0x4c494146

	adbVersion = 0x01000000
	adbMaxData = 4096
	headSize   = 24
	ioTimeout  = 20000 // ms

	voiceRemoteDir = "/tmp/mavo-call"
)

type usb struct {
	ctx       *C.libusb_context
	handle    *C.libusb_device_handle
	iface     int
	in, out   byte
	remoteMax uint32
	nextID    uint32
	mu        sync.Mutex
}

func openModule() (*usb, error) {
	var ctx *C.libusb_context
	if rc := C.libusb_init(&ctx); rc != 0 {
		return nil, fmt.Errorf("libusb_init: %d", rc)
	}
	var list **C.libusb_device
	n := C.libusb_get_device_list(ctx, &list)
	var handle *C.libusb_device_handle
	for _, dev := range unsafe.Slice(list, int(n)) {
		var d C.struct_libusb_device_descriptor
		if C.libusb_get_device_descriptor(dev, &d) != 0 {
			continue
		}
		if uint16(d.idVendor) != vendorID || uint16(d.idProduct) != productID {
			continue
		}
		if C.libusb_open(dev, &handle) == 0 {
			break
		}
		handle = nil
	}
	C.libusb_free_device_list(list, 1)
	if handle == nil {
		C.libusb_exit(ctx)
		return nil, fmt.Errorf("no USB device %04x:%04x", vendorID, productID)
	}

	var cfg *C.struct_libusb_config_descriptor
	if C.libusb_get_active_config_descriptor(C.libusb_get_device(handle), &cfg) != 0 {
		C.libusb_close(handle)
		C.libusb_exit(ctx)
		return nil, errors.New("no active config descriptor")
	}
	defer C.libusb_free_config_descriptor(cfg)

	ifaces := unsafe.Slice(cfg._interface, int(cfg.bNumInterfaces))
	var iface int = -1
	var epIn, epOut byte
	for _, itf := range ifaces {
		alts := unsafe.Slice(itf.altsetting, int(itf.num_altsetting))
		for _, alt := range alts {
			if byte(alt.bInterfaceSubClass) != 0x42 || byte(alt.bInterfaceProtocol) != 0x01 {
				continue
			}
			for _, ep := range unsafe.Slice(alt.endpoint, int(alt.bNumEndpoints)) {
				if byte(ep.bmAttributes)&0x03 != 0x02 { // bulk
					continue
				}
				addr := byte(ep.bEndpointAddress)
				if addr&0x80 != 0 {
					epIn = addr
				} else {
					epOut = addr
				}
			}
			if epIn != 0 && epOut != 0 {
				iface = int(alt.bInterfaceNumber)
				break
			}
		}
	}
	if iface < 0 {
		C.libusb_close(handle)
		C.libusb_exit(ctx)
		return nil, errors.New("no ADB interface (0xff/0x42/0x01)")
	}
	if rc := C.libusb_claim_interface(handle, C.int(iface)); rc != 0 {
		C.libusb_close(handle)
		C.libusb_exit(ctx)
		return nil, fmt.Errorf("claim interface %d: %d", iface, rc)
	}
	return &usb{ctx: ctx, handle: handle, iface: iface, in: epIn, out: epOut,
		remoteMax: adbMaxData, nextID: 1}, nil
}

func (u *usb) close() {
	if u.handle != nil {
		C.libusb_release_interface(u.handle, C.int(u.iface))
		C.libusb_close(u.handle)
	}
	if u.ctx != nil {
		C.libusb_exit(u.ctx)
	}
}

func (u *usb) bulkWrite(b []byte) error {
	buf := C.CBytes(b)
	defer C.free(buf)
	var sent C.int
	rc := C.libusb_bulk_transfer(u.handle, C.uint8_t(u.out), (*C.uchar)(buf), C.int(len(b)), &sent, ioTimeout)
	if rc != 0 {
		return fmt.Errorf("bulk write: %d", rc)
	}
	return nil
}

func (u *usb) bulkRead(n int) ([]byte, error) {
	buf := make([]byte, n)
	var got C.int
	rc := C.libusb_bulk_transfer(u.handle, C.uint8_t(u.in), (*C.uchar)(unsafe.Pointer(&buf[0])), C.int(n), &got, ioTimeout)
	if rc != 0 {
		return nil, fmt.Errorf("bulk read: %d", rc)
	}
	return buf[:int(got)], nil
}

// writeMessage writes header and payload as separate transfers — the whole
// trick. Doing it in one transfer is what makes this module's adbd go silent.
func (u *usb) writeMessage(cmd, arg0, arg1 uint32, payload []byte) error {
	h := make([]byte, headSize)
	binary.LittleEndian.PutUint32(h[0:4], cmd)
	binary.LittleEndian.PutUint32(h[4:8], arg0)
	binary.LittleEndian.PutUint32(h[8:12], arg1)
	binary.LittleEndian.PutUint32(h[12:16], uint32(len(payload)))
	var sum uint32
	for _, b := range payload {
		sum += uint32(b)
	}
	binary.LittleEndian.PutUint32(h[16:20], sum)
	binary.LittleEndian.PutUint32(h[20:24], cmd^0xffffffff)
	if err := u.bulkWrite(h); err != nil {
		return err
	}
	if len(payload) > 0 {
		return u.bulkWrite(payload)
	}
	return nil
}

func (u *usb) readMessage() (cmd, arg0, arg1 uint32, payload []byte, err error) {
	h, err := u.bulkRead(headSize)
	if err != nil {
		return 0, 0, 0, nil, err
	}
	if len(h) != headSize {
		return 0, 0, 0, nil, fmt.Errorf("short header %d", len(h))
	}
	cmd = binary.LittleEndian.Uint32(h[0:4])
	arg0 = binary.LittleEndian.Uint32(h[4:8])
	arg1 = binary.LittleEndian.Uint32(h[8:12])
	n := binary.LittleEndian.Uint32(h[12:16])
	if n > 1<<20 {
		return 0, 0, 0, nil, fmt.Errorf("payload too big: %d", n)
	}
	if n > 0 {
		payload, err = u.bulkRead(int(n))
		if err != nil {
			return 0, 0, 0, nil, err
		}
	}
	return cmd, arg0, arg1, payload, nil
}

func (u *usb) connect() error {
	if err := u.writeMessage(adbCNXN, adbVersion, adbMaxData, []byte("host::\x00")); err != nil {
		return err
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		cmd, _, arg1, _, err := u.readMessage()
		if err != nil {
			return err
		}
		switch cmd {
		case adbCNXN:
			if arg1 > 0 {
				u.remoteMax = arg1
			}
			return nil
		case adbAUTH:
			return errors.New("module asked for ADB AUTH")
		}
	}
	return errors.New("timeout waiting for CNXN")
}

func (u *usb) open(service string) (uint32, error) {
	id := u.nextID
	u.nextID++
	return u.openNamed(id, service)
}

func (u *usb) openNamed(id uint32, service string) (uint32, error) {
	if err := u.writeMessage(adbOPEN, id, 0, append([]byte(service), 0)); err != nil {
		return 0, err
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		cmd, arg0, arg1, _, err := u.readMessage()
		if err != nil {
			return 0, err
		}
		if cmd == adbOKAY && arg1 == id {
			return arg0, nil
		}
		if cmd == adbCLSE {
			return 0, fmt.Errorf("module refused %s", service)
		}
	}
	return 0, fmt.Errorf("timeout opening %s", service)
}

// shell runs one command and returns its output.
func (u *usb) shell(command string) (string, error) {
	local, err := u.open("shell:" + command)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		cmd, arg0, arg1, payload, err := u.readMessage()
		if err != nil {
			return out.String(), err
		}
		switch cmd {
		case adbWRTE:
			out.Write(payload)
			_ = u.writeMessage(adbOKAY, arg1, arg0, nil)
		case adbCLSE:
			if arg0 == local || arg1 == local {
				return out.String(), nil
			}
		case adbOKAY:
			// stream handshake
		}
	}
	return out.String(), errors.New("shell timeout")
}

// readSyncReply waits for a 4-byte sync-level reply (OKAY / FAIL) that the
// device delivers inside a WRTE message; the WRTE must be acknowledged.
func (u *usb) readSyncReply(local, remote uint32, timeout time.Duration) (uint32, string, error) {
	deadline := time.Now().Add(timeout)
	var buf []byte
	for time.Now().Before(deadline) {
		cmd, arg0, arg1, payload, err := u.readMessage()
		if err != nil {
			return 0, "", err
		}
		if os.Getenv("QDC507_DEBUG") != "" {
			fmt.Fprintf(os.Stderr, "[adb] cmd=%08x arg0=%d arg1=%d len=%d\n", cmd, arg0, arg1, len(payload))
		}
		switch cmd {
		case adbWRTE:
			_ = u.writeMessage(adbOKAY, local, remote, nil)
			buf = append(buf, payload...)
			if len(buf) < 4 {
				continue
			}
			kind := binary.LittleEndian.Uint32(buf[0:4])
			switch kind {
			case adbOKAY:
				return kind, "", nil
			case syncFAIL:
				if len(buf) >= 8 {
					n := binary.LittleEndian.Uint32(buf[4:8])
					if len(buf) >= 8+int(n) {
						return kind, string(buf[8 : 8+n]), nil
					}
				}
			}
		case adbOKAY:
			_ = arg0
		case adbCLSE:
			if arg0 == local || arg1 == local {
				return 0, "", errors.New("sync stream closed")
			}
		}
	}
	return 0, "", errors.New("sync reply timeout")
}

// pushFile sends one file via the ADB sync protocol. Sync records travel as
// WRTE payloads on the "sync:" stream and each one is acknowledged.
func (u *usb) pushFile(localPath, remotePath string, mode uint32) error {
	data, err := os.ReadFile(localPath)
	if err != nil {
		return err
	}
	local := u.nextID
	u.nextID++
	remote, err := u.openNamed(local, "sync:")
	if err != nil {
		return err
	}

	send := make([]byte, 0, 8+len(remotePath)+16)
	hdr := make([]byte, 8)
	binary.LittleEndian.PutUint32(hdr[0:4], syncSEND)
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(len(remotePath)+1+len(fmt.Sprintf("%d", mode))))
	send = append(send, hdr...)
	send = append(send, []byte(remotePath)...)
	send = append(send, ',')
	send = append(send, []byte(fmt.Sprintf("%d", mode))...)
	send = append(send, 0)
	if err := u.writeMessage(adbWRTE, local, remote, send); err != nil {
		return err
	}
	if kind, msg, err := u.readSyncReply(local, remote, 20*time.Second); err != nil || kind != adbOKAY {
		return fmt.Errorf("sync SEND rejected (%v, kind=%08x %s)", err, kind, msg)
	}

	const chunk = 32 * 1024
	for off := 0; off < len(data); off += chunk {
		end := off + chunk
		if end > len(data) {
			end = len(data)
		}
		part := data[off:end]
		rec := make([]byte, 8+len(part))
		binary.LittleEndian.PutUint32(rec[0:4], syncDATA)
		binary.LittleEndian.PutUint32(rec[4:8], uint32(len(part)))
		copy(rec[8:], part)
		if err := u.writeMessage(adbWRTE, local, remote, rec); err != nil {
			return err
		}
		if kind, msg, err := u.readSyncReply(local, remote, 20*time.Second); err != nil || kind != adbOKAY {
			return fmt.Errorf("sync DATA rejected at %d (%v, kind=%08x %s)", off, err, kind, msg)
		}
	}

	done := make([]byte, 8)
	binary.LittleEndian.PutUint32(done[0:4], syncDONE)
	binary.LittleEndian.PutUint32(done[4:8], uint32(time.Now().Unix()))
	if err := u.writeMessage(adbWRTE, local, remote, done); err != nil {
		return err
	}
	if kind, msg, err := u.readSyncReply(local, remote, 20*time.Second); err != nil || kind != adbOKAY {
		return fmt.Errorf("sync DONE rejected (%v, kind=%08x %s)", err, kind, msg)
	}
	_ = u.writeMessage(adbCLSE, local, remote, nil)
	fmt.Printf("  pushed %s (%d bytes, mode %o)\n", remotePath, len(data), mode)
	return nil
}

func main() {
	mode := flag.String("state", "", "push | shell | start | stop | status")
	flag.Parse()

	u, err := openModule()
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	defer u.close()
	if err := u.connect(); err != nil {
		fmt.Fprintln(os.Stderr, "adb connect:", err)
		os.Exit(1)
	}

	switch {
	case *mode == "push" && flag.NArg() > 0:
		dir := flag.Arg(0)
		if _, err := u.shell("mkdir -p " + voiceRemoteDir + " && chmod 700 " + voiceRemoteDir); err != nil {
			fmt.Fprintln(os.Stderr, "mkdir:", err)
			os.Exit(1)
		}
		for _, name := range []string{"qdc507_aprv3.ko", "qdc507_voice.ko", "mavo-pcm-bridge.armv7"} {
			m := uint32(0o644)
			if strings.HasSuffix(name, ".armv7") {
				m = 0o755
			}
			if err := u.pushFile(filepath.Join(dir, name), voiceRemoteDir+"/"+name, m); err != nil {
				fmt.Fprintln(os.Stderr, "push", name, ":", err)
				os.Exit(1)
			}
		}
		fmt.Println("push complete")

	case *mode == "shell" && flag.NArg() > 0:
		out, err := u.shell(strings.Join(flag.Args(), " "))
		fmt.Print(out)
		if err != nil {
			fmt.Fprintln(os.Stderr, "shell:", err)
			os.Exit(1)
		}

	case *mode == "status":
		out, _ := u.shell("id -u; uname -r; ls -l " + voiceRemoteDir + " 2>/dev/null; ls /dev/snd 2>/dev/null | head; lsmod | head -5")
		fmt.Print(out)

	case *mode == "start":
		for _, cmd := range []string{
			"insmod " + voiceRemoteDir + "/qdc507_aprv3.ko",
			"insmod " + voiceRemoteDir + "/qdc507_voice.ko",
			"lsmod | grep -E 'qdc507' ",
			"ls /dev/snd",
		} {
			out, err := u.shell(cmd)
			fmt.Printf("$ %s\n%s", cmd, out)
			if err != nil {
				fmt.Fprintln(os.Stderr, err)
			}
		}

	default:
		flag.Usage()
		os.Exit(2)
	}
}
