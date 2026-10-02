package modem

import (
	"testing"
)

func TestParseURC(t *testing.T) {
	cases := []struct {
		line string
		want EventType
		num  string
		idx  int
		rssi int
		ok   bool
	}{
		{"RING", EventIncomingCall, "", 0, 0, true},
		{`+CLIP: "13800138000",129,,,,0`, EventIncomingCall, "13800138000", 0, 0, true},
		{"NO CARRIER", EventCallEnded, "NO CARRIER", 0, 0, true},
		{"BUSY", EventCallEnded, "BUSY", 0, 0, true},
		{`+CMTI: "ME",3`, EventIncomingSMS, "", 3, 0, true},
		{"+CSQ: 20,0", EventSignal, "", 0, 20, true},
		{"AT", "", "", 0, 0, false},
		{"OK", "", "", 0, 0, false},
		{"+CEREG: 1", "", "", 0, 0, false},
	}
	for _, c := range cases {
		ev, ok := ParseURC(c.line)
		if ok != c.ok {
			t.Errorf("ParseURC(%q) ok=%v, want %v", c.line, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		if ev.Type != c.want || ev.Number != c.num || ev.Index != c.idx || ev.RSSI != c.rssi {
			t.Errorf("ParseURC(%q) = %+v, want type=%s num=%q idx=%d rssi=%d",
				c.line, ev, c.want, c.num, c.idx, c.rssi)
		}
	}
}

func TestIsFinalResult(t *testing.T) {
	for _, l := range []string{"OK", "ERROR", "NO CARRIER", "+CME ERROR: 3", "+CMS ERROR: 500"} {
		if !IsFinalResult(l) {
			t.Errorf("IsFinalResult(%q) = false, want true", l)
		}
	}
	if IsFinalResult("+CLIP: \"123\"") {
		t.Errorf("IsFinalResult(+CLIP) = true, want false")
	}
}
