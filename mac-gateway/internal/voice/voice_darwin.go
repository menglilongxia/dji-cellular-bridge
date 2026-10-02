//go:build darwin && cgo

// Package voice bridges the cellular module's USB Audio (UAC) interface with
// the gateway's RTP path. Only usable on macOS, and only after the module's
// USB composition has been switched on — see docs/HARDWARE-SPIKE.md.
package voice

/*
#cgo CFLAGS: -I${SRCDIR}
#cgo LDFLAGS: -framework CoreAudio -framework AudioToolbox -framework CoreFoundation
#include <stdlib.h>
#include "coreaudio_darwin.h"
*/
import "C"

import (
	"fmt"
	"sync"
	"sync/atomic"
	"unsafe"
)

// The C side calls back into this exported function from the CoreAudio thread.
// `ctx` is an opaque integer handle (never a Go pointer), so the Go GC is not
// asked to track a pointer living in C memory.
//
//export cellbridgeVoiceFrame
func cellbridgeVoiceFrame(pcm *C.int16_t, frames C.int, ctx unsafe.Pointer) {
	id := uintptr(ctx)
	v, ok := devices.Load(id)
	if !ok {
		return
	}
	d := v.(*Device)
	if d.onMic == nil || frames <= 0 {
		return
	}
	n := int(frames)
	// Copy: the buffer is only valid for the duration of the callback.
	src := unsafe.Slice((*int16)(unsafe.Pointer(pcm)), n)
	buf := make([]int16, n)
	copy(buf, src)
	d.onMic(buf)
}

var (
	devices    sync.Map // uintptr -> *Device
	deviceID   atomic.Uintptr
	deviceIncr = uintptr(1)
)

// Device is an open audio bridge to the module.
type Device struct {
	h      *C.voice_handle
	id     uintptr
	onMic  func([]int16)
	closeM sync.Once
}

// Devices lists audio devices (optionally filtered by a name substring).
func Devices(substr string) (string, error) {
	var csub *C.char
	if substr != "" {
		csub = C.CString(substr)
		defer C.free(unsafe.Pointer(csub))
	}
	buf := make([]byte, 4096)
	n := C.voice_list_devices(csub, (*C.char)(unsafe.Pointer(&buf[0])), C.int(len(buf)))
	if n < 0 {
		return "", fmt.Errorf("voice: list devices: %s", C.GoString(C.voice_last_error()))
	}
	return string(buf[:n]), nil
}

// Open starts capture+playback on the device whose name contains nameSubstr.
// onMic receives 8 kHz mono int16 frames (160 samples per RTP frame, though a
// driver chunk may contain several).
func Open(nameSubstr string, onMic func([]int16)) (*Device, error) {
	return open(nameSubstr, onMic, true)
}

// OpenCapture is Open without the playback half, for microphone-permission
// diagnostics.
func OpenCapture(nameSubstr string, onMic func([]int16)) (*Device, error) {
	return open(nameSubstr, onMic, false)
}

func open(nameSubstr string, onMic func([]int16), withPlayback bool) (*Device, error) {
	cname := C.CString(nameSubstr)
	defer C.free(unsafe.Pointer(cname))

	d := &Device{onMic: onMic, id: 0}
	// Hand out a small non-zero integer as the opaque handle.
	d.id = deviceID.Add(deviceIncr)
	devices.Store(d.id, d)

	var h *C.voice_handle
	if withPlayback {
		h = C.voice_open(cname, C.intptr_t(d.id))
	} else {
		h = C.voice_open_capture(cname, C.intptr_t(d.id))
	}
	if h == nil {
		devices.Delete(d.id)
		return nil, fmt.Errorf("voice: open %q: %s", nameSubstr, C.GoString(C.voice_last_error()))
	}
	d.h = h
	return d, nil
}

// Play queues 8 kHz mono int16 PCM for playback to the module.
func (d *Device) Play(pcm []int16) error {
	if d == nil || d.h == nil || len(pcm) == 0 {
		return nil
	}
	rc := C.voice_play(d.h, (*C.int16_t)(unsafe.Pointer(&pcm[0])), C.int(len(pcm)))
	if rc != 0 {
		return fmt.Errorf("voice: play failed")
	}
	return nil
}

// Close stops both directions.
func (d *Device) Close() {
	if d == nil {
		return
	}
	d.closeM.Do(func() {
		if d.h != nil {
			C.voice_close(d.h)
			d.h = nil
		}
		if d.id != 0 {
			devices.Delete(d.id)
			d.id = 0
		}
	})
}
