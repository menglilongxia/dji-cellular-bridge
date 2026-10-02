package sip

import (
	"strings"
	"testing"
)

func TestParseRequest(t *testing.T) {
	raw := "INVITE sip:13800138000@gateway SIP/2.0\r\n" +
		"Via: SIP/2.0/UDP 192.168.1.2:5060;branch=z9hG4bKabc\r\n" +
		"From: <sip:iphone@gateway>;tag=111\r\n" +
		"To: <sip:13800138000@gateway>\r\n" +
		"Call-ID: xyz@gateway\r\n" +
		"CSeq: 1 INVITE\r\n" +
		"Content-Type: application/sdp\r\n" +
		"Content-Length: 11\r\n" +
		"\r\n" +
		"v=0\r\nhello"
	m, err := ParseMessage([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if !m.IsRequest || m.Method != "INVITE" || m.RequestURI != "sip:13800138000@gateway" {
		t.Fatalf("bad request line: %+v", m)
	}
	if m.GetHeader("via") != "SIP/2.0/UDP 192.168.1.2:5060;branch=z9hG4bKabc" {
		t.Fatalf("via mismatch: %q", m.GetHeader("Via"))
	}
	if m.Body != "v=0\r\nhello" {
		t.Fatalf("body mismatch: %q", m.Body)
	}
}

func TestParseResponse(t *testing.T) {
	raw := "SIP/2.0 200 OK\r\nCall-ID: xyz\r\nContent-Length: 0\r\n\r\n"
	m, err := ParseMessage([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if m.IsRequest || m.StatusCode != 200 || m.Reason != "OK" {
		t.Fatalf("bad status line: %+v", m)
	}
}

func TestSerializeRoundtrip(t *testing.T) {
	m := NewRequest("MESSAGE", "sip:10086@gateway")
	m.SetHeader("From", "<sip:iphone@gateway>;tag=a1")
	m.SetHeader("To", "<sip:10086@gateway>")
	m.SetHeader("Call-ID", "c1@gw")
	m.SetHeader("CSeq", "1 MESSAGE")
	m.Body = "hello 短信"
	s := m.Serialize()
	if !strings.Contains(s, "Content-Length: 12\r\n") { // "hello 短信" = 5 + 1 + 2*3 = 12 bytes
		t.Fatalf("content-length wrong:\n%s", s)
	}
	m2, err := ParseMessage([]byte(s))
	if err != nil {
		t.Fatal(err)
	}
	if m2.Method != "MESSAGE" || m2.Body != "hello 短信" || m2.GetHeader("Call-ID") != "c1@gw" {
		t.Fatalf("roundtrip mismatch: %+v", m2)
	}
}

func TestDigestRoundtrip(t *testing.T) {
	const user, pass, realm, nonce, method, uri = "iphone", "s3cret", "dji-cellular-bridge", "n0nce", "REGISTER", "sip:gateway"
	hdr := BuildAuthHeader(user, pass, realm, nonce, method, uri)
	if !VerifyDigest(user, pass, realm, method, hdr) {
		t.Fatalf("digest verify failed for %q", hdr)
	}
	if VerifyDigest(user, "wrong", realm, method, hdr) {
		t.Fatalf("digest verify passed with wrong password")
	}
}
