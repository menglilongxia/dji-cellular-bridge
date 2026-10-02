//go:build !(darwin && cgo)

package voice

import "errors"

// The audio bridge needs macOS CoreAudio; elsewhere the gateway stays on the
// RTP loopback path.
type Device struct{}

var errUnavailable = errors.New("voice: CoreAudio bridge needs darwin + cgo")

func Devices(substr string) (string, error) { return "", errUnavailable }

func Open(nameSubstr string, onMic func([]int16)) (*Device, error) {
	return nil, errUnavailable
}

func (d *Device) Play(pcm []int16) error { return errUnavailable }
func (d *Device) Close()                 {}
