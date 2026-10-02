// voiceprobe inspects and exercises the CoreAudio bridge to the cellular
// module's USB Audio interface.
//
//	voiceprobe list                 # list audio devices
//	voiceprobe open [nameSubstring] # open capture+playback for ~3s and report levels
package main

import (
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"github.com/dji-cellular-bridge/mac-gateway/internal/voice"
)

func main() {
	cmd := "list"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "list":
		out, err := voice.Devices("")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Print(out)

	case "open":
		substr := "BAIWANG"
		if len(os.Args) > 2 {
			substr = os.Args[2]
		}
		secs := 3.0
		if len(os.Args) > 3 {
			fmt.Sscanf(os.Args[3], "%f", &secs)
		}
		var frames, peak atomic.Int64
		start := time.Now()
		dev, err := voice.Open(substr, func(pcm []int16) {
			frames.Add(int64(len(pcm)))
			for _, s := range pcm {
				v := int64(s)
				if v < 0 {
					v = -v
				}
				for {
					old := peak.Load()
					if v <= old || peak.CompareAndSwap(old, v) {
						break
					}
				}
			}
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("opened %q; capturing for %.0fs…\n", substr, secs)
		time.Sleep(time.Duration(secs * float64(time.Second)))
		dev.Close()
		elapsed := time.Since(start).Seconds()
		n := frames.Load()
		fmt.Printf("captured %d frames in %.1fs (%d frames/s, expect ~8000)\n", n, elapsed, int(float64(n)/elapsed))
		fmt.Printf("peak amplitude: %d (silence would be ~0)\n", peak.Load())

	default:
		fmt.Fprintln(os.Stderr, "usage: voiceprobe [list|open [nameSubstring]]")
		os.Exit(2)
	}
}
