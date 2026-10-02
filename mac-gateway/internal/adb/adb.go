// Package adb implements the host side of the ADB wire protocol on top of a
// caller-supplied USB transport.
//
// The DJI QDC507 (and its siblings) ship an adbd that desktop adb refuses to
// talk to: its vendor id is not in adb's device list. Speaking the protocol
// ourselves is therefore the only way in — and it has to happen inside the
// gateway process, because the gateway is the only owner of the USB device.
//
// Two details of this module's adbd cost hours and are load-bearing:
//
//  1. The payload checksum in the 24-byte header must be the real sum. A zero
//     checksum is dropped silently — the write succeeds and the device never
//     answers, which looks exactly like "adbd is not running".
//  2. The header and the payload must be two separate USB bulk transfers.
//     Merging them into one transfer is ignored the same way.
//
// Both follow from the Transport contract below: one Write call is exactly one
// USB transfer, and Client always issues two of them.
package adb

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// ADB message commands.
const (
	cmdCNXN = 0x4e584e43 // CNXN
	cmdAUTH = 0x48545541 // AUTH
	cmdOPEN = 0x4e45504f // OPEN
	cmdOKAY = 0x59414b4f // OKAY
	cmdCLSE = 0x45534c43 // CLSE
	cmdWRTE = 0x45545257 // WRTE
)

const (
	// Version is the ADB protocol version this firmware accepts. Desktop adb
	// sends 0x01000001; this adbd answers nothing at all unless it is 0x01000000.
	Version = 0x01000000
	// MaxData is how much payload we let the device put in one WRTE.
	MaxData = 4096

	headSize   = 24
	maxPayload = 1 << 20
)

// DefaultBanner is the host banner. "host::" (no feature list) is what this
// adbd expects; a features string makes it skip the connection.
const DefaultBanner = "host::"

var (
	// ErrAuthRequired means adbd wants an RSA key exchange, which we do not do.
	ErrAuthRequired = errors.New("adb: device requested authentication")
	// ErrTimeout means the device did not answer in time.
	ErrTimeout = errors.New("adb: timed out waiting for the device")
	// ErrServiceClosed means adbd closed the stream we asked for. On this
	// module that is the signature of a wedged adbd: every service is refused
	// until the USB device is reset.
	ErrServiceClosed = errors.New("adb: service closed by device")
)

// Transport is a pair of bulk pipes to the module's adbd.
//
// Implementations must send each Write as a single USB bulk transfer: this
// module merges nothing. Read must return (0, nil) when the timeout expires
// with no data, and must not block for longer than the timeout.
type Transport interface {
	Write(p []byte) error
	Read(p []byte, timeout time.Duration) (int, error)
}

// Client speaks ADB over one Transport. It is safe for concurrent use, but
// only one shell command runs at a time (adbd multiplexes streams; we do not).
type Client struct {
	tr Transport

	mu        sync.Mutex
	connected bool
	nextID    uint32
	remoteMax uint32
}

// New wraps tr in a client. Call Connect before Shell.
func New(tr Transport) *Client {
	return &Client{tr: tr, nextID: 1, remoteMax: MaxData}
}

// Connected reports whether a CNXN handshake has completed.
func (c *Client) Connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected
}

// Reset forgets the connection so the next command reconnects. Call it after
// the underlying USB device has been reset or re-enumerated.
func (c *Client) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resetLocked()
}

// resetLocked is Reset with c.mu already held.
func (c *Client) resetLocked() {
	c.connected = false
	c.nextID = 1
}

// Connect performs the CNXN handshake. It is idempotent: a second call on an
// already connected client is a no-op, which is what keeps the gateway from
// re-opening a session per call — repeated session churn is what wedges adbd.
func (c *Client) Connect(ctx context.Context, timeout time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.connected {
		return nil
	}
	if err := c.writeMessage(cmdCNXN, Version, MaxData, append([]byte(DefaultBanner), 0)); err != nil {
		return fmt.Errorf("adb: send CNXN: %w", err)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		cmd, _, arg1, _, err := c.readPacket(ctx, time.Until(deadline))
		if err != nil {
			c.resetLocked()
			return fmt.Errorf("adb: read CNXN reply: %w", err)
		}
		switch cmd {
		case cmdCNXN:
			if arg1 > 0 {
				c.remoteMax = arg1
			}
			c.connected = true
			return nil
		case cmdAUTH:
			return ErrAuthRequired
		}
		// Anything else is noise from a previous session; keep waiting.
	}
	return fmt.Errorf("adb: %w (CNXN)", ErrTimeout)
}

// EnsureConnected connects if needed. Cheap to call before every command.
func (c *Client) EnsureConnected(ctx context.Context) error {
	return c.Connect(ctx, 20*time.Second)
}

// Shell runs one command through the "shell:" service and returns its combined
// output. This adbd predates shell_v2, so it reports no exit status: callers
// have to look at the output.
func (c *Client) Shell(ctx context.Context, command string, timeout time.Duration) (string, error) {
	if err := c.EnsureConnected(ctx); err != nil {
		return "", err
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	local := c.nextID
	c.nextID++
	if err := c.writeMessage(cmdOPEN, local, 0, append([]byte("shell:"+command), 0)); err != nil {
		c.resetLocked()
		return "", fmt.Errorf("adb: open shell: %w", err)
	}

	var (
		out      strings.Builder
		remote   uint32
		deadline = time.Now().Add(timeout)
		// opened records that adbd accepted the stream. It matters: a command
		// that prints nothing (mkdir, pkill, insmod) also ends with CLSE, and
		// treating that as a refusal makes every silent success look like a
		// wedged adbd.
		opened bool
	)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			return out.String(), fmt.Errorf("adb: %w (shell %q)", ErrTimeout, abbreviated(command))
		}
		cmd, arg0, _, payload, err := c.readPacket(ctx, left)
		if err != nil {
			// A read that failed mid-message leaves the stream desynchronised,
			// so the session is gone: reconnect on the next command instead of
			// talking nonsense into a stale one.
			c.resetLocked()
			return out.String(), err
		}
		switch cmd {
		case cmdOKAY:
			remote = arg0
			opened = true
		case cmdWRTE:
			if remote == 0 {
				remote = arg0
			}
			opened = true
			out.Write(payload)
			if err := c.writeMessage(cmdOKAY, local, remote, nil); err != nil {
				return out.String(), err
			}
		case cmdCLSE:
			// Acknowledge so adbd can release the stream. An immediate CLSE
			// without an OKAY is exactly how a wedged adbd looks.
			_ = c.writeMessage(cmdCLSE, local, arg0, nil)
			if !opened {
				return "", fmt.Errorf("%w (shell %q)", ErrServiceClosed, abbreviated(command))
			}
			return out.String(), nil
		case cmdAUTH:
			return out.String(), ErrAuthRequired
		}
	}
}

// ShellContains runs a command and reports whether its output contains want.
// It exists for the many "is this already done?" checks the provisioner makes.
func (c *Client) ShellContains(ctx context.Context, command, want string, timeout time.Duration) (bool, error) {
	out, err := c.Shell(ctx, command, timeout)
	if err != nil {
		return false, err
	}
	return strings.Contains(out, want), nil
}

// Reboot asks adbd to reboot the module. It returns as soon as the request is
// on the wire, because the device usually disappears before it can answer.
//
// This is the way back from a wedged adbd. The module has ~43 MB of RAM and
// its shell service stops being able to fork long before anything else breaks;
// the reboot service forks nothing, so it still works when every "shell:"
// open is refused, and a reboot restores a working shell.
func (c *Client) Reboot(ctx context.Context, timeout time.Duration) error {
	if err := c.EnsureConnected(ctx); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	local := c.nextID
	c.nextID++
	if err := c.writeMessage(cmdOPEN, local, 0, append([]byte("reboot:"), 0)); err != nil {
		return fmt.Errorf("adb: send reboot: %w", err)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		cmd, arg0, _, payload, err := c.readPacket(ctx, time.Until(deadline))
		if err != nil {
			// The module went down while answering — that is the point.
			return nil
		}
		switch cmd {
		case cmdOKAY:
			// service accepted
		case cmdWRTE:
			_ = payload
			_ = c.writeMessage(cmdOKAY, local, arg0, nil)
		case cmdCLSE:
			_ = c.writeMessage(cmdCLSE, local, arg0, nil)
			return nil
		}
	}
	return nil
}

// --- wire format -----------------------------------------------------------

func (c *Client) writeMessage(cmd, arg0, arg1 uint32, payload []byte) error {
	h := make([]byte, headSize)
	binary.LittleEndian.PutUint32(h[0:4], cmd)
	binary.LittleEndian.PutUint32(h[4:8], arg0)
	binary.LittleEndian.PutUint32(h[8:12], arg1)
	binary.LittleEndian.PutUint32(h[12:16], uint32(len(payload)))
	binary.LittleEndian.PutUint32(h[16:20], checksum(payload))
	binary.LittleEndian.PutUint32(h[20:24], cmd^0xffffffff)

	// Two transfers, never one: see the package comment.
	if err := c.tr.Write(h); err != nil {
		return err
	}
	if len(payload) > 0 {
		return c.tr.Write(payload)
	}
	return nil
}

func checksum(p []byte) uint32 {
	var sum uint32
	for _, b := range p {
		sum += uint32(b)
	}
	return sum
}

func (c *Client) readPacket(ctx context.Context, timeout time.Duration) (cmd, arg0, arg1 uint32, payload []byte, err error) {
	select {
	case <-ctx.Done():
		return 0, 0, 0, nil, ctx.Err()
	default:
	}
	h := make([]byte, headSize)
	if err := c.readExact(h, timeout); err != nil {
		return 0, 0, 0, nil, err
	}
	cmd = binary.LittleEndian.Uint32(h[0:4])
	arg0 = binary.LittleEndian.Uint32(h[4:8])
	arg1 = binary.LittleEndian.Uint32(h[8:12])
	n := binary.LittleEndian.Uint32(h[12:16])
	if n > maxPayload {
		return 0, 0, 0, nil, fmt.Errorf("adb: payload too large: %d", n)
	}
	if n > 0 {
		payload = make([]byte, n)
		if err := c.readExact(payload, timeout); err != nil {
			return 0, 0, 0, nil, err
		}
	}
	return cmd, arg0, arg1, payload, nil
}

func (c *Client) readExact(p []byte, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for off := 0; off < len(p); {
		left := time.Until(deadline)
		if left <= 0 {
			return ErrTimeout
		}
		n, err := c.tr.Read(p[off:], left)
		if err != nil {
			return err
		}
		if n == 0 {
			// A transport that reports a timeout as (0, nil) would otherwise
			// spin; give the deadline a chance to pass.
			time.Sleep(2 * time.Millisecond)
			continue
		}
		off += n
	}
	return nil
}

func abbreviated(cmd string) string {
	const max = 48
	flat := strings.Join(strings.Fields(cmd), " ")
	if len(flat) <= max {
		return flat
	}
	return flat[:max] + "…"
}
