// Package rtp implements RFC 3550 RTP packet framing and G.711 mu-law
// (PCMU) conversion. Voice in this project is always PCMU/8000, 20ms
// frames (160 samples), matching the SIP/SDP profile in docs/PROTOCOL.md.
package rtp

import (
	"encoding/binary"
	"errors"
	"net"
	"sync"
	"sync/atomic"
)

// Packet is one RTP packet.
type Packet struct {
	Marker      bool
	PayloadType uint8
	Sequence    uint16
	Timestamp   uint32
	SSRC        uint32
	Payload     []byte
}

// Marshal encodes the packet (12-byte fixed header, no extensions/CSRC).
func (p *Packet) Marshal() []byte {
	b := make([]byte, 12+len(p.Payload))
	b[0] = 0x80
	if p.Marker {
		b[1] |= 0x80
	}
	b[1] |= p.PayloadType & 0x7F
	binary.BigEndian.PutUint16(b[2:], p.Sequence)
	binary.BigEndian.PutUint32(b[4:], p.Timestamp)
	binary.BigEndian.PutUint32(b[8:], p.SSRC)
	copy(b[12:], p.Payload)
	return b
}

// ParsePacket decodes one RTP packet.
func ParsePacket(b []byte) (*Packet, error) {
	if len(b) < 12 {
		return nil, errors.New("rtp: packet too short")
	}
	if b[0]>>6 != 2 {
		return nil, errors.New("rtp: bad version")
	}
	p := &Packet{
		Marker:      b[1]&0x80 != 0,
		PayloadType: b[1] & 0x7F,
		Sequence:    binary.BigEndian.Uint16(b[2:]),
		Timestamp:   binary.BigEndian.Uint32(b[4:]),
		SSRC:        binary.BigEndian.Uint32(b[8:]),
	}
	cc := int(b[0] & 0x0F)
	hdrLen := 12 + cc*4
	if b[0]&0x10 != 0 { // extension
		if len(b) < hdrLen+4 {
			return nil, errors.New("rtp: bad extension")
		}
		extLen := int(binary.BigEndian.Uint16(b[hdrLen+2:])) * 4
		hdrLen += 4 + extLen
	}
	if len(b) < hdrLen {
		return nil, errors.New("rtp: truncated")
	}
	p.Payload = b[hdrLen:]
	return p, nil
}

// --- G.711 mu-law ---

// LinearToUlaw encodes 16-bit linear PCM to 8-bit mu-law.
func LinearToUlaw(sample int16) byte {
	const (
		bias = 0x84
		clip = 32635
	)
	sign := 0
	s := int(sample)
	if s < 0 {
		s = -s
		sign = 0x80
	}
	if s > clip {
		s = clip
	}
	s += bias
	exp := 0
	for tmp := s >> 7; tmp > 1; tmp >>= 1 {
		exp++
	}
	mantissa := (s >> (exp + 3)) & 0x0F
	return byte(^(sign | (exp << 4) | mantissa))
}

// UlawToLinear decodes 8-bit mu-law to 16-bit linear PCM.
func UlawToLinear(ulaw byte) int16 {
	u := int(^ulaw)
	sign := u & 0x80
	exp := (u >> 4) & 0x07
	mantissa := u & 0x0F
	sample := ((mantissa << 3) + 0x84) << exp
	sample -= 0x84
	if sign != 0 {
		sample = -sample
	}
	return int16(sample)
}

// EncodeFrame converts 160 PCM samples (20ms @ 8kHz) to a PCMU payload.
func EncodeFrame(pcm []int16) []byte {
	out := make([]byte, len(pcm))
	for i, s := range pcm {
		out[i] = LinearToUlaw(s)
	}
	return out
}

// DecodeFrame converts a PCMU payload to PCM samples.
func DecodeFrame(payload []byte) []int16 {
	out := make([]int16, len(payload))
	for i, b := range payload {
		out[i] = UlawToLinear(b)
	}
	return out
}

// --- RTP session ---

// Session is a UDP endpoint for RTP. In loopback mode it echoes every
// received datagram back to its sender (used with FakeModem: lets the
// phone verify the audio path without real hardware).
type Session struct {
	conn     *net.UDPConn
	loopback bool
	closed   chan struct{}
	once     sync.Once

	// OnPacket receives the payload of every RTP packet (nil in loopback mode).
	OnPacket func(payload []byte, src *net.UDPAddr)

	// rawIn counts every datagram that arrived, parseable or not, and lastFrom
	// remembers the most recent sender. "The phone says it sent" and "we
	// received" have to be distinguishable when a call is silent.
	rawIn    atomic.Int64
	lastFrom atomic.Value // string
}

// RawReceived returns how many datagrams reached this socket, and the last
// sender address (empty when nothing has arrived).
func (s *Session) RawReceived() (int64, string) {
	if s == nil {
		return 0, ""
	}
	from, _ := s.lastFrom.Load().(string)
	return s.rawIn.Load(), from
}

// Listen binds a UDP socket for RTP.
func Listen(port int, loopback bool) (*Session, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: port})
	if err != nil {
		return nil, err
	}
	return &Session{conn: conn, loopback: loopback, closed: make(chan struct{})}, nil
}

// LocalPort returns the bound port.
func (s *Session) LocalPort() int {
	if a, ok := s.conn.LocalAddr().(*net.UDPAddr); ok {
		return a.Port
	}
	return 0
}

// Run pumps packets until Close. Blocking.
func (s *Session) Run() {
	buf := make([]byte, 2048)
	for {
		n, src, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-s.closed:
				return
			default:
				continue
			}
		}
		if n == 0 {
			continue
		}
		s.rawIn.Add(1)
		if src != nil {
			s.lastFrom.Store(src.String())
		}
		if s.loopback {
			_, _ = s.conn.WriteToUDP(buf[:n], src)
			continue
		}
		if s.OnPacket != nil {
			if pkt, err := ParsePacket(buf[:n]); err == nil {
				s.OnPacket(pkt.Payload, src)
			}
		}
	}
}

// Send writes one RTP packet with the given payload to dst.
func (s *Session) Send(payload []byte, dst *net.UDPAddr) error {
	seq, ts, ssrc := s.nextHeader()
	pkt := &Packet{Sequence: seq, Timestamp: ts, SSRC: ssrc, Payload: payload}
	_, err := s.conn.WriteToUDP(pkt.Marshal(), dst)
	return err
}

var (
	headerMu   sync.Mutex
	headerSeq  uint16
	headerTS   uint32
	headerSSRC uint32 = 0x43425247 // "CBRG"
)

func (s *Session) nextHeader() (uint16, uint32, uint32) {
	headerMu.Lock()
	defer headerMu.Unlock()
	seq := headerSeq
	ts := headerTS
	headerSeq++
	headerTS += 160 // 20 ms @ 8 kHz
	return seq, ts, headerSSRC
}

// Close stops the session.
func (s *Session) Close() {
	s.once.Do(func() {
		close(s.closed)
		s.conn.Close()
	})
}
