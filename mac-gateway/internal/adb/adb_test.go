package adb

import (
	"context"
	"encoding/binary"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAdbd emulates the parts of the module's adbd that the client depends on,
// including the two quirks: header and payload must arrive as separate
// transfers, and the payload checksum must be real.
type fakeAdbd struct {
	mu      sync.Mutex
	rx      []byte
	tx      [][]byte
	merged  bool // a write carried more than a bare header
	badSum  bool // a message arrived with a zero checksum and a payload
	cnxn    int  // CNXN handshakes seen
	handler func(service string) []byte
}

func newFakeAdbd(handler func(string) []byte) *fakeAdbd {
	return &fakeAdbd{handler: handler}
}

func (f *fakeAdbd) Write(p []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(p) > headSize {
		f.merged = true
	}
	f.rx = append(f.rx, p...)
	f.consume()
	return nil
}

func (f *fakeAdbd) consume() {
	for len(f.rx) >= headSize {
		h := f.rx[:headSize]
		cmd := binary.LittleEndian.Uint32(h[0:4])
		n := int(binary.LittleEndian.Uint32(h[12:16]))
		sum := binary.LittleEndian.Uint32(h[16:20])
		if len(f.rx) < headSize+n {
			return
		}
		payload := f.rx[headSize : headSize+n]
		f.rx = f.rx[headSize+n:]
		if n > 0 && sum == 0 {
			f.badSum = true
			continue // a zero checksum: silently dropped, no reply at all
		}
		switch cmd {
		case cmdCNXN:
			f.cnxn++
			f.push(message(cmdCNXN, 0, MaxData, []byte("device::ro.product.name=qdc507;")))
		case cmdOPEN:
			service := strings.TrimRight(string(payload), "\x00")
			if f.handler != nil {
				f.push(message(cmdOKAY, 7, 0, nil))
				if body := f.handler(service); body != nil {
					f.push(message(cmdWRTE, 7, 0, body))
				}
				f.push(message(cmdCLSE, 7, 0, nil))
			} else {
				f.push(message(cmdCLSE, 0, 0, nil)) // wedged adbd
			}
		case cmdOKAY:
			if binary.LittleEndian.Uint32(h[4:8]) == 0 { // our CLSE ack
				f.push(message(cmdCLSE, 7, 0, nil))
			}
		}
	}
}

func (f *fakeAdbd) push(b []byte) { f.tx = append(f.tx, b) }

func (f *fakeAdbd) Read(p []byte, timeout time.Duration) (int, error) {
	deadline := time.Now().Add(timeout)
	for {
		f.mu.Lock()
		if len(f.tx) > 0 {
			msg := f.tx[0]
			f.tx = f.tx[1:]
			f.mu.Unlock()
			n := copy(p, msg)
			if n < len(msg) {
				f.mu.Lock()
				f.tx = append([][]byte{msg[n:]}, f.tx...)
				f.mu.Unlock()
			}
			return n, nil
		}
		f.mu.Unlock()
		if time.Now().After(deadline) {
			return 0, nil
		}
		time.Sleep(time.Millisecond)
	}
}

func message(cmd, arg0, arg1 uint32, payload []byte) []byte {
	h := make([]byte, headSize)
	binary.LittleEndian.PutUint32(h[0:4], cmd)
	binary.LittleEndian.PutUint32(h[4:8], arg0)
	binary.LittleEndian.PutUint32(h[8:12], arg1)
	binary.LittleEndian.PutUint32(h[12:16], uint32(len(payload)))
	binary.LittleEndian.PutUint32(h[16:20], checksum(payload))
	binary.LittleEndian.PutUint32(h[20:24], cmd^0xffffffff)
	return append(h, payload...)
}

func TestShellRoundTrip(t *testing.T) {
	dev := newFakeAdbd(func(service string) []byte {
		if !strings.HasPrefix(service, "shell:") {
			t.Errorf("unexpected service %q", service)
		}
		return []byte("uid=0(root)\n")
	})
	c := New(dev)
	ctx := context.Background()

	if err := c.EnsureConnected(ctx); err != nil {
		t.Fatalf("connect: %v", err)
	}
	out, err := c.Shell(ctx, "id -u", 2*time.Second)
	if err != nil {
		t.Fatalf("shell: %v", err)
	}
	if !strings.Contains(out, "uid=0") {
		t.Fatalf("unexpected output %q", out)
	}

	dev.mu.Lock()
	defer dev.mu.Unlock()
	if dev.merged {
		t.Error("client merged header and payload into one transfer")
	}
	if dev.badSum {
		t.Error("client sent a payload with a zero checksum")
	}
}

func TestShellReportsClosedService(t *testing.T) {
	dev := newFakeAdbd(nil) // every OPEN is refused: a wedged adbd
	c := New(dev)
	_, err := c.Shell(context.Background(), "id", 2*time.Second)
	if !errors.Is(err, ErrServiceClosed) {
		t.Fatalf("want ErrServiceClosed, got %v", err)
	}
}

// A command that prints nothing must not look like a refused service: adbd
// accepts the stream (OKAY) and only then closes it.
func TestSilentCommandSucceeds(t *testing.T) {
	dev := newFakeAdbd(func(string) []byte { return nil })
	c := New(dev)
	out, err := c.Shell(context.Background(), "mkdir -p /tmp/x", 2*time.Second)
	if err != nil {
		t.Fatalf("silent command reported an error: %v", err)
	}
	if out != "" {
		t.Fatalf("want empty output, got %q", out)
	}
}

func TestConnectAndShellShareOneSession(t *testing.T) {
	dev := newFakeAdbd(func(string) []byte { return []byte("ok\n") })
	c := New(dev)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := c.Shell(ctx, "true", 2*time.Second); err != nil {
			t.Fatalf("shell %d: %v", i, err)
		}
	}
	// One CNXN: the connection is reused instead of reopened per command.
	dev.mu.Lock()
	defer dev.mu.Unlock()
	if dev.cnxn != 1 {
		t.Fatalf("want 1 CNXN handshake, got %d", dev.cnxn)
	}
	if !c.Connected() {
		t.Fatal("client should stay connected")
	}
}
