package rtp

import (
	"math"
	"testing"
)

func TestPacketRoundtrip(t *testing.T) {
	p := &Packet{Marker: true, PayloadType: 0, Sequence: 1234, Timestamp: 567890, SSRC: 0xdeadbeef, Payload: []byte{1, 2, 3, 4}}
	q, err := ParsePacket(p.Marshal())
	if err != nil {
		t.Fatal(err)
	}
	if q.Marker != p.Marker || q.PayloadType != p.PayloadType || q.Sequence != p.Sequence ||
		q.Timestamp != p.Timestamp || q.SSRC != p.SSRC || string(q.Payload) != string(p.Payload) {
		t.Fatalf("roundtrip mismatch: %+v", q)
	}
}

func TestUlawVectors(t *testing.T) {
	if got := LinearToUlaw(0); got != 0xFF {
		t.Errorf("ulaw(0) = %#02x, want 0xff", got)
	}
	if got := LinearToUlaw(32635); got != 0x80 {
		t.Errorf("ulaw(32635) = %#02x, want 0x80", got)
	}
	if got := UlawToLinear(0xFF); got != 0 {
		t.Errorf("linear(0xff) = %d, want 0", got)
	}
}

func TestUlawRoundtripQuality(t *testing.T) {
	// mu-law is lossy; error should stay within ~4% of magnitude.
	for _, s := range []int16{100, 1000, 8000, 16000, 30000, -5000, -20000} {
		back := UlawToLinear(LinearToUlaw(s))
		diff := math.Abs(float64(back) - float64(s))
		if tol := math.Abs(float64(s)) * 0.04; diff > tol {
			t.Errorf("ulaw roundtrip(%d) = %d, diff %v > tol %v", s, back, diff, tol)
		}
	}
}
