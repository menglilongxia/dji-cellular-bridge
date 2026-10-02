package sip

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// SDPInfo is the minimal SDP we care about: where to send RTP.
type SDPInfo struct {
	IP   string
	Port int
}

// BuildSDP creates a minimal audio-only SDP offering PCMU/8000.
func BuildSDP(ip string, port int) string {
	id := randHex(8)
	return fmt.Sprintf("v=0\r\n"+
		"o=- %s %s IN IP4 %s\r\n"+
		"s=-\r\n"+
		"c=IN IP4 %s\r\n"+
		"t=0 0\r\n"+
		"m=audio %d RTP/AVP 0\r\n"+
		"a=rtpmap:0 PCMU/8000\r\n", id, id, ip, ip, port)
}

// ParseSDP extracts the first audio media endpoint.
func ParseSDP(body string) (*SDPInfo, error) {
	info := &SDPInfo{}
	for _, ln := range strings.Split(body, "\n") {
		ln = strings.TrimSpace(ln)
		if strings.HasPrefix(ln, "c=IN IP4 ") {
			info.IP = strings.TrimSpace(strings.TrimPrefix(ln, "c=IN IP4 "))
		}
		if strings.HasPrefix(ln, "m=audio ") {
			f := strings.Fields(ln)
			if len(f) >= 2 {
				info.Port, _ = strconv.Atoi(f[1])
			}
		}
	}
	if info.Port == 0 {
		return nil, errors.New("sip: no m=audio in SDP")
	}
	if info.IP == "" || info.IP == "0.0.0.0" {
		info.IP = "127.0.0.1"
	}
	return info, nil
}
