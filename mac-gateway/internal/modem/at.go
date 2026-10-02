package modem

import (
	"regexp"
	"strconv"
	"strings"
)

var (
	clipRe = regexp.MustCompile(`\+CLIP:\s*"([^"]+)"`)
	cmtiRe = regexp.MustCompile(`\+CMTI:\s*"[^"]*",\s*(\d+)`)
	csqRe  = regexp.MustCompile(`\+CSQ:\s*(\d+)`)
)

// ParseURC interprets one modem line as an unsolicited event.
// ok=false for command echoes, final results and unknown lines.
func ParseURC(line string) (ev Event, ok bool) {
	line = strings.TrimSpace(line)
	switch {
	case line == "RING":
		return Event{Type: EventIncomingCall}, true
	case strings.HasPrefix(line, "+CLIP:"):
		ev := Event{Type: EventIncomingCall}
		if m := clipRe.FindStringSubmatch(line); m != nil {
			ev.Number = m[1]
		}
		return ev, true
	case line == "NO CARRIER" || line == "BUSY" || line == "NO ANSWER" || line == "NO DIALTONE":
		return Event{Type: EventCallEnded, Number: line}, true
	case strings.HasPrefix(line, "+CMTI:"):
		ev := Event{Type: EventIncomingSMS}
		if m := cmtiRe.FindStringSubmatch(line); m != nil {
			ev.Index, _ = strconv.Atoi(m[1])
		}
		return ev, true
	case strings.HasPrefix(line, "+CSQ:"):
		ev := Event{Type: EventSignal, RSSI: 99}
		if m := csqRe.FindStringSubmatch(line); m != nil {
			ev.RSSI, _ = strconv.Atoi(m[1])
		}
		return ev, true
	}
	return Event{}, false
}

// IsFinalResult reports AT final result codes.
func IsFinalResult(line string) bool {
	line = strings.TrimSpace(line)
	switch line {
	case "OK", "ERROR", "NO CARRIER", "BUSY", "NO ANSWER", "NO DIALTONE":
		return true
	}
	return strings.HasPrefix(line, "+CME ERROR") || strings.HasPrefix(line, "+CMS ERROR")
}
