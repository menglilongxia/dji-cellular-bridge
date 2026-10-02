//go:build darwin && cgo

package modem

import (
	"os"
	"strings"
	"testing"
	"time"
)

// Live hardware test. It talks to the real QDC507 plugged into this Mac, so it
// only runs when explicitly enabled:
//
//	CELLBRIDGE_AT_LIVE=1 go test ./internal/modem/ -run TestUSBATLive -v
func TestUSBATLive(t *testing.T) {
	if os.Getenv("CELLBRIDGE_AT_LIVE") != "1" {
		t.Skip("set CELLBRIDGE_AT_LIVE=1 to talk to the attached module")
	}

	var (
		u   *USBAT
		err error
	)
	for _, vid := range QDC507VendorIDs {
		if u, err = OpenUSBAT(vid); err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer u.Close()

	if got := sendAT(t, u, "AT"); !strings.Contains(got, "OK") {
		t.Fatalf("AT did not answer OK, got %q", got)
	}

	ident := sendAT(t, u, "ATI")
	t.Logf("ATI =>\n%s", ident)
	if !strings.Contains(ident, "QDC507") {
		t.Errorf("expected QDC507 in ATI output, got %q", ident)
	}

	sig := sendAT(t, u, "AT+CSQ")
	t.Logf("AT+CSQ => %s", strings.TrimSpace(sig))
	if !strings.Contains(sig, "+CSQ:") {
		t.Errorf("expected +CSQ in output, got %q", sig)
	}
}

func sendAT(t *testing.T, u *USBAT, cmd string) string {
	t.Helper()
	if err := u.Write([]byte(cmd + "\r")); err != nil {
		t.Fatalf("write %q: %v", cmd, err)
	}
	var sb strings.Builder
	deadline := time.Now().Add(5 * time.Second)
	buf := make([]byte, 1024)
	for time.Now().Before(deadline) {
		n, err := u.ReadAT(buf, 1000)
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if n > 0 {
			sb.Write(buf[:n])
			if strings.Contains(sb.String(), "OK\r\n") || strings.Contains(sb.String(), "ERROR") {
				break
			}
		}
	}
	return sb.String()
}
