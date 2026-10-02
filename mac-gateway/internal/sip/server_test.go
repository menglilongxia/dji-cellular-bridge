package sip

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// capture records outbound SIP messages without touching the network.
type capture struct {
	mu   sync.Mutex
	msgs []*Message
	ch   chan *Message
}

func newCapture() *capture { return &capture{ch: make(chan *Message, 128)} }

func (c *capture) sendFunc() func(*Message, *net.UDPAddr) {
	return func(m *Message, _ *net.UDPAddr) {
		c.mu.Lock()
		c.msgs = append(c.msgs, m)
		c.mu.Unlock()
		select {
		case c.ch <- m:
		default:
		}
	}
}

func (c *capture) next(t *testing.T, timeout time.Duration) *Message {
	t.Helper()
	select {
	case m := <-c.ch:
		return m
	case <-time.After(timeout):
		t.Fatal("timed out waiting for outbound message")
		return nil
	}
}

var testSrc = &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 5061}

func newTestServer(cb Callbacks) (*Server, *capture) {
	srv := NewServer(Config{
		Realm: "test-realm", Username: "iphone", Password: "pw",
		AdvertiseIP: "127.0.0.1", LoopbackRTP: false,
	}, cb)
	cap := newCapture()
	srv.sendFunc = cap.sendFunc()
	return srv, cap
}

func registerMsg(cseq int, auth string) *Message {
	r := NewRequest("REGISTER", "sip:gateway")
	r.SetHeader("Via", "SIP/2.0/UDP 127.0.0.1:5061;branch="+branch())
	r.SetHeader("From", "<sip:iphone@gateway>;tag=client1")
	r.SetHeader("To", "<sip:iphone@gateway>")
	r.SetHeader("Call-ID", "reg1@test")
	r.SetHeader("CSeq", fmt.Sprintf("%d REGISTER", cseq))
	r.SetHeader("Contact", "<sip:iphone@127.0.0.1:5061>")
	r.SetHeader("Expires", "3600")
	r.SetHeader("X-Push-Token", "test-voip-token")
	if auth != "" {
		r.SetHeader("Authorization", auth)
	}
	return r
}

func doRegister(t *testing.T, srv *Server, cap *capture) {
	t.Helper()
	srv.dispatch(registerMsg(1, ""), testSrc)
	chal := cap.next(t, 2*time.Second)
	if chal.StatusCode != 401 {
		t.Fatalf("expected 401, got %d", chal.StatusCode)
	}
	nonce := ParseAuthHeader(chal.GetHeader("WWW-Authenticate"))["nonce"]
	auth := BuildAuthHeader("iphone", "pw", "test-realm", nonce, "REGISTER", "sip:gateway")
	srv.dispatch(registerMsg(2, auth), testSrc)
	ok := cap.next(t, 2*time.Second)
	if ok.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", ok.StatusCode)
	}
	if !srv.HasBinding() {
		t.Fatal("no binding after REGISTER")
	}
	if srv.PushToken() != "test-voip-token" {
		t.Fatalf("push token not stored: %q", srv.PushToken())
	}
}

func TestRegister(t *testing.T) {
	srv, cap := newTestServer(Callbacks{})
	doRegister(t, srv, cap)

	// Wrong password -> 403.
	bad := registerMsg(3, BuildAuthHeader("iphone", "wrong", "test-realm", "n", "REGISTER", "sip:gateway"))
	srv.dispatch(bad, testSrc)
	if r := cap.next(t, 2*time.Second); r.StatusCode != 403 {
		t.Fatalf("expected 403, got %d", r.StatusCode)
	}
}

func TestIncomingCall(t *testing.T) {
	answered := make(chan *Session, 1)
	ended := make(chan string, 1)
	srv, cap := newTestServer(Callbacks{
		OnAnswered: func(s *Session) { answered <- s },
		OnEnded:    func(s *Session, r string) { ended <- r },
	})
	doRegister(t, srv, cap)

	sessCh := make(chan *Session, 1)
	errCh := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		sess, err := srv.InviteClient(ctx, "13800138000")
		if err != nil {
			errCh <- err
			return
		}
		sessCh <- sess
	}()

	inv := cap.next(t, 3*time.Second)
	if !inv.IsRequest || inv.Method != "INVITE" {
		t.Fatalf("expected INVITE, got %+v", inv)
	}
	if !strings.Contains(inv.GetHeader("From"), "13800138000") {
		t.Fatalf("caller not in From: %q", inv.GetHeader("From"))
	}
	callID := inv.GetHeader("Call-ID")
	serverTag := parseTag(inv.GetHeader("From"))
	if _, err := ParseSDP(inv.Body); err != nil {
		t.Fatalf("INVITE has no SDP: %v", err)
	}

	reply := func(status int, reason string, withSDP bool) {
		r := NewResponse(status, reason)
		r.SetHeader("Via", inv.GetHeader("Via"))
		r.SetHeader("From", inv.GetHeader("From"))
		r.SetHeader("To", inv.GetHeader("To")+";tag=phone1")
		r.SetHeader("Call-ID", callID)
		r.SetHeader("CSeq", inv.GetHeader("CSeq"))
		if withSDP {
			r.SetHeader("Content-Type", "application/sdp")
			r.Body = BuildSDP("127.0.0.1", 42000)
		}
		srv.dispatch(r, testSrc)
	}
	reply(100, "Trying", false)
	reply(180, "Ringing", false)
	reply(200, "OK", true)

	ack := cap.next(t, 3*time.Second)
	if !ack.IsRequest || ack.Method != "ACK" {
		t.Fatalf("expected ACK, got %+v", ack)
	}
	var sess *Session
	select {
	case sess = <-sessCh:
	case err := <-errCh:
		t.Fatalf("InviteClient: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("InviteClient never returned")
	}
	select {
	case <-answered:
	case <-time.After(3 * time.Second):
		t.Fatal("OnAnswered never fired")
	}
	if sess.State != StateActive || sess.RemoteRTP() == nil || sess.RemoteRTP().Port != 42000 {
		t.Fatalf("bad session state: %+v", sess)
	}

	// Client hangs up.
	bye := NewRequest("BYE", "sip:13800138000@127.0.0.1")
	bye.SetHeader("Via", "SIP/2.0/UDP 127.0.0.1:5061;branch="+branch())
	bye.SetHeader("From", "<sip:iphone@gateway>;tag=phone1")
	bye.SetHeader("To", fmt.Sprintf("<sip:13800138000@gateway>;tag=%s", serverTag))
	bye.SetHeader("Call-ID", callID)
	bye.SetHeader("CSeq", "2 BYE")
	srv.dispatch(bye, testSrc)
	if r := cap.next(t, 3*time.Second); r.StatusCode != 200 {
		t.Fatalf("BYE -> %d", r.StatusCode)
	}
	select {
	case <-ended:
	case <-time.After(3 * time.Second):
		t.Fatal("OnEnded never fired")
	}
	if len(srv.ActiveSessions()) != 0 {
		t.Fatal("session not cleaned up")
	}
}

func TestOutgoingCall(t *testing.T) {
	srv, cap := newTestServer(Callbacks{})
	doRegister(t, srv, cap)

	callID := "out1@test"
	inv := NewRequest("INVITE", "sip:13900001111@127.0.0.1")
	inv.SetHeader("Via", "SIP/2.0/UDP 127.0.0.1:5061;branch="+branch())
	inv.SetHeader("From", "<sip:iphone@gateway>;tag=phone2")
	inv.SetHeader("To", "<sip:13900001111@gateway>")
	inv.SetHeader("Call-ID", callID)
	inv.SetHeader("CSeq", "1 INVITE")
	inv.SetHeader("Contact", "<sip:iphone@127.0.0.1:5061>")
	inv.SetHeader("Content-Type", "application/sdp")
	inv.Body = BuildSDP("127.0.0.1", 42002)
	srv.dispatch(inv, testSrc)

	if r := cap.next(t, 3*time.Second); r.StatusCode != 100 {
		t.Fatalf("expected 100, got %d", r.StatusCode)
	}
	ring := cap.next(t, 3*time.Second)
	if ring.StatusCode != 180 {
		t.Fatalf("expected 180, got %d", ring.StatusCode)
	}
	serverTag := parseTag(ring.GetHeader("To"))
	if serverTag == "" {
		t.Fatal("no server tag in 180")
	}

	var sess *Session
	for _, s := range srv.ActiveSessions() {
		if s.Number == "13900001111" {
			sess = s
		}
	}
	if sess == nil {
		t.Fatal("no session for outgoing call")
	}
	sess.Accept() // modem connected
	ok := cap.next(t, 3*time.Second)
	if ok.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", ok.StatusCode)
	}
	if _, err := ParseSDP(ok.Body); err != nil {
		t.Fatalf("200 OK has no SDP: %v", err)
	}

	ack := NewRequest("ACK", "sip:13900001111@127.0.0.1")
	ack.SetHeader("Via", "SIP/2.0/UDP 127.0.0.1:5061;branch="+branch())
	ack.SetHeader("From", "<sip:iphone@gateway>;tag=phone2")
	ack.SetHeader("To", fmt.Sprintf("<sip:13900001111@gateway>;tag=%s", serverTag))
	ack.SetHeader("Call-ID", callID)
	ack.SetHeader("CSeq", "1 ACK")
	srv.dispatch(ack, testSrc)
	time.Sleep(100 * time.Millisecond)
	if sess.State != StateActive {
		t.Fatalf("session state = %s, want active", sess.State)
	}

	// Duplicate INVITE (client retransmit) must not create a second session.
	srv.dispatch(inv, testSrc)
	if dup := cap.next(t, 3*time.Second); dup.StatusCode != 200 {
		t.Fatalf("duplicate INVITE -> %d, want 200", dup.StatusCode)
	}
	if len(srv.ActiveSessions()) != 1 {
		t.Fatalf("duplicate INVITE created extra sessions: %d", len(srv.ActiveSessions()))
	}

	// Server-initiated BYE (modem hung up).
	sess.Bye()
	bye := cap.next(t, 3*time.Second)
	if !bye.IsRequest || bye.Method != "BYE" {
		t.Fatalf("expected BYE, got %+v", bye)
	}
	if len(srv.ActiveSessions()) != 0 {
		t.Fatal("session not cleaned up after BYE")
	}
}

func TestMessage(t *testing.T) {
	type smsT struct{ from, to, body string }
	smsCh := make(chan smsT, 1)
	srv, cap := newTestServer(Callbacks{
		OnMessage: func(from, to, body string) { smsCh <- smsT{from, to, body} },
	})
	doRegister(t, srv, cap)

	msg := NewRequest("MESSAGE", "sip:10086@127.0.0.1")
	msg.SetHeader("Via", "SIP/2.0/UDP 127.0.0.1:5061;branch="+branch())
	msg.SetHeader("From", "<sip:iphone@gateway>;tag=phone3")
	msg.SetHeader("To", "<sip:10086@gateway>")
	msg.SetHeader("Call-ID", "msg1@test")
	msg.SetHeader("CSeq", "2 MESSAGE")
	msg.SetHeader("Content-Type", "text/plain; charset=utf-8")
	msg.Body = "查话费"
	srv.dispatch(msg, testSrc)
	if r := cap.next(t, 3*time.Second); r.StatusCode != 200 {
		t.Fatalf("MESSAGE -> %d", r.StatusCode)
	}
	select {
	case m := <-smsCh:
		if m.to != "10086" || m.body != "查话费" {
			t.Fatalf("sms mismatch: %+v", m)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("OnMessage never fired")
	}

	// Server -> client MESSAGE (incoming SMS path).
	if err := srv.SendMessageToClient("10086", "余额 42 元"); err != nil {
		t.Fatal(err)
	}
	out := cap.next(t, 3*time.Second)
	if !out.IsRequest || out.Method != "MESSAGE" || out.Body != "余额 42 元" {
		t.Fatalf("bad outbound MESSAGE: %+v", out)
	}
}
