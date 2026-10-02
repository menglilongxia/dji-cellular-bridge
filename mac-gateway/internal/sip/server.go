package sip

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dji-cellular-bridge/mac-gateway/internal/rtp"
)

// Config for the SIP server.
type Config struct {
	Addr        string // UDP listen address, e.g. ":5060"
	Realm       string
	Username    string
	Password    string
	AdvertiseIP string // IP placed in SDP/Via; "" = auto-detect
	RTPBase     int    // first RTP port to allocate (default 40000)
	LoopbackRTP bool   // echo RTP (fake-audio mode, no hardware)
}

// Binding is a registered phone client.
type Binding struct {
	User      string
	Addr      *net.UDPAddr
	PushToken string // from X-Push-Token header on REGISTER
	ExpiresAt time.Time
}

// Direction of a call.
type Direction string

const (
	DirIncoming Direction = "in"  // modem -> phone
	DirOutgoing Direction = "out" // phone -> modem
)

// State of a call session.
type State string

const (
	StateRinging State = "ringing"
	StateActive  State = "active"
	StateEnded   State = "ended"
)

// Callbacks wire the SIP server to the gateway controller.
type Callbacks struct {
	OnOutgoingCall func(sess *Session) // client dialed; controller should Dial() the modem
	OnAnswered     func(sess *Session) // voice path may start
	OnEnded        func(sess *Session, reason string)
	OnMessage      func(from, to, body string) // client -> gateway SMS
	OnRegistered   func(b *Binding)
}

// Session is one call leg between gateway and phone.
type Session struct {
	ID        string
	CallID    string
	Direction Direction
	Number    string // peer phone number
	State     State

	mu         sync.Mutex
	srv        *Server
	client     *net.UDPAddr
	clientURI  string
	cseq       int
	serverTag  string
	clientTag  string
	inviteReq  *Message // original INVITE (outgoing direction)
	ok200      *Message // last 200 OK sent (retransmitted on duplicate INVITE)
	rtpPort    int
	rtpSess    *rtp.Session
	rtpStarted bool
	remoteRTP  *net.UDPAddr

	done chan struct{}
	once sync.Once
}

// Server is a minimal SIP UDP server for exactly one phone client.
type Server struct {
	cfg  Config
	cb   Callbacks
	conn *net.UDPConn

	mu            sync.Mutex
	binding       *Binding
	sessions      map[string]*Session
	byCallID      map[string]*Session
	pendingInvite map[string]chan *Message // Call-ID -> response waiter
	nextRTP       int

	// sendFunc transports messages; set by Run (UDP), injectable in tests.
	sendFunc func(msg *Message, dst *net.UDPAddr)

	closed chan struct{}
	wg     sync.WaitGroup
}

func NewServer(cfg Config, cb Callbacks) *Server {
	if cfg.Realm == "" {
		cfg.Realm = "dji-cellular-bridge"
	}
	if cfg.RTPBase == 0 {
		cfg.RTPBase = 40000
	}
	return &Server{
		cfg:           cfg,
		cb:            cb,
		sessions:      make(map[string]*Session),
		byCallID:      make(map[string]*Session),
		pendingInvite: make(map[string]chan *Message),
		nextRTP:       cfg.RTPBase,
		closed:        make(chan struct{}),
	}
}

// Run starts the UDP server; returns when ctx is done.
func (s *Server) Run(ctx context.Context) error {
	addr, err := net.ResolveUDPAddr("udp", s.cfg.Addr)
	if err != nil {
		return err
	}
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		return err
	}
	s.conn = conn
	s.sendFunc = func(msg *Message, dst *net.UDPAddr) {
		_, _ = conn.WriteToUDP([]byte(msg.Serialize()), dst)
	}
	defer conn.Close()

	go func() {
		<-ctx.Done()
		close(s.closed)
		conn.Close()
	}()

	buf := make([]byte, 65535)
	for {
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-s.closed:
				return nil
			default:
				continue
			}
		}
		msg, err := ParseMessage(buf[:n])
		if err != nil {
			log.Printf("sip: parse error from %s: %v", src, err)
			continue
		}
		s.dispatch(msg, src)
	}
}

// HasBinding reports whether a client is currently registered.
func (s *Server) HasBinding() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.binding != nil && time.Now().Before(s.binding.ExpiresAt)
}

// PushToken returns the registered client's VoIP push token (may be "").
func (s *Server) PushToken() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.binding == nil {
		return ""
	}
	return s.binding.PushToken
}

// ActiveSessions for status/debugging.
func (s *Server) ActiveSessions() []*Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		out = append(out, sess)
	}
	return out
}

func (s *Server) send(msg *Message, dst *net.UDPAddr) {
	if s.sendFunc == nil {
		return
	}
	s.sendFunc(msg, dst)
}

func (s *Server) advertiseIP() string {
	if s.cfg.AdvertiseIP != "" {
		return s.cfg.AdvertiseIP
	}
	// Use the IP that matches the registered client's network.
	// If the client comes from Tailscale (100.x), advertise our Tailscale IP.
	// If from LAN (192.168.x), advertise our LAN IP.
	s.mu.Lock()
	b := s.binding
	s.mu.Unlock()
	if b != nil && b.Addr != nil {
		return bestLocalIPFor(b.Addr.IP)
	}
	return localIP()
}

// bestLocalIPFor returns the local address on the same network as the peer.
func bestLocalIPFor(peer net.IP) string {
	if peer == nil {
		return localIP()
	}
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.To4() == nil || ipnet.IP.IsLoopback() {
			continue
		}
		if ipnet.Contains(peer) {
			return ipnet.IP.String()
		}
	}
	// Fallback: if peer is 100.x (Tailscale), find our 100.x
	peerStr := peer.String()
	if strings.HasPrefix(peerStr, "100.") {
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok || ipnet.IP.To4() == nil {
				continue
			}
			if strings.HasPrefix(ipnet.IP.String(), "100.") {
				return ipnet.IP.String()
			}
		}
	}
	return localIP()
}

func (s *Server) listenPort() int {
	_, p, err := net.SplitHostPort(s.cfg.Addr)
	if err != nil {
		return 5060
	}
	port, _ := strconv.Atoi(p)
	return port
}

func (s *Server) advertiseAddr() string {
	return fmt.Sprintf("%s:%d", s.advertiseIP(), s.listenPort())
}

// respond replies to a request, copying Via/From/Call-ID/CSeq.
func (s *Server) respond(req *Message, dst *net.UDPAddr, status int, reason string, extra func(*Message)) {
	resp := NewResponse(status, reason)
	resp.SetHeader("Via", req.GetHeader("Via"))
	resp.SetHeader("From", req.GetHeader("From"))
	resp.SetHeader("To", req.GetHeader("To"))
	resp.SetHeader("Call-ID", req.GetHeader("Call-ID"))
	resp.SetHeader("CSeq", req.GetHeader("CSeq"))
	if extra != nil {
		extra(resp)
	}
	s.send(resp, dst)
}

func (s *Server) dispatch(msg *Message, src *net.UDPAddr) {
	if !msg.IsRequest {
		s.handleResponse(msg)
		return
	}
	switch msg.Method {
	case "REGISTER":
		s.handleRegister(msg, src)
	case "INVITE":
		s.handleInvite(msg, src)
	case "ACK":
		s.handleAck(msg)
	case "BYE":
		s.handleBye(msg, src)
	case "CANCEL":
		s.handleCancel(msg, src)
	case "MESSAGE":
		s.handleMessageReq(msg, src)
	case "OPTIONS":
		s.respond(msg, src, 200, "OK", nil)
	default:
		s.respond(msg, src, 405, "Method Not Allowed", nil)
	}
}

func (s *Server) handleResponse(msg *Message) {
	callID := msg.GetHeader("Call-ID")
	s.mu.Lock()
	wait, ok := s.pendingInvite[callID]
	sess := s.byCallID[callID]
	s.mu.Unlock()

	if ok {
		select {
		case wait <- msg:
		default:
		}
		return
	}
	// Duplicate 200 OK for an answered session (our ACK was lost): resend ACK.
	if sess != nil && msg.StatusCode >= 200 && msg.StatusCode < 300 &&
		msg.GetHeader("CSeq") == fmt.Sprintf("%d INVITE", sess.cseq) {
		sess.mu.Lock()
		active := sess.State == StateActive
		sess.mu.Unlock()
		if active {
			s.sendAck(sess, msg)
		}
	}
}

// --- REGISTER ---

func (s *Server) handleRegister(req *Message, src *net.UDPAddr) {
	user := sipUser(req.GetHeader("To"))
	if user == "" {
		user = sipUser(req.GetHeader("From"))
	}
	if user == "" {
		user = s.cfg.Username
	}
	if req.GetHeader("Authorization") == "" {
		s.respond(req, src, 401, "Unauthorized", func(r *Message) {
			r.SetHeader("WWW-Authenticate", Challenge(s.cfg.Realm, NewNonce()))
		})
		return
	}
	if !VerifyDigest(s.cfg.Username, s.cfg.Password, s.cfg.Realm, "REGISTER", req.GetHeader("Authorization")) {
		s.respond(req, src, 403, "Forbidden", nil)
		return
	}
	host, port := parseContact(req.GetHeader("Contact"), src)
	exp := parseExpires(req.GetHeader("Expires"))
	// Call back to the address the REGISTER actually came from, not the one the
	// client advertises. The iPhone app picked its cellular PDP address
	// (192.0.0.6) as "local IP" while really registering from Wi-Fi
	// (192.168.1.122), so INVITEs sent to the advertised address never arrived.
	// This is the usual NAT-friendly behaviour for a SIP registrar.
	callback := src
	if callback == nil {
		callback = &net.UDPAddr{IP: net.ParseIP(host), Port: port}
	} else if callback.Port == 0 {
		callback = &net.UDPAddr{IP: callback.IP, Port: port}
	}
	s.mu.Lock()
	s.binding = &Binding{
		User:      user,
		Addr:      callback,
		PushToken: req.GetHeader("X-Push-Token"),
		ExpiresAt: time.Now().Add(exp),
	}
	b := s.binding
	s.mu.Unlock()
	s.respond(req, src, 200, "OK", func(r *Message) {
		r.SetHeader("Contact", fmt.Sprintf("<sip:%s@%s>", user, s.advertiseAddr()))
		r.SetHeader("Expires", strconv.Itoa(int(exp.Seconds())))
	})
	// Print the Contact-derived callback address *and* the actual REGISTER
	// source: if they differ, the client's advertised port is wrong and
	// INVITEs will go nowhere.
	log.Printf("sip: registered %s contact=%s src=%s (push=%v)",
		user, b.Addr, src, b.PushToken != "")
	if s.cb.OnRegistered != nil {
		go s.cb.OnRegistered(b)
	}
}

// --- INVITE ---

// ErrNoClient is returned by InviteClient when no phone is registered.
var ErrNoClient = errors.New("sip: no registered client")

// InviteClient rings the phone for an incoming modem call.
// It blocks until answered, rejected, or ctx expires.
func (s *Server) InviteClient(ctx context.Context, number string) (*Session, error) {
	s.mu.Lock()
	b := s.binding
	if b == nil || time.Now().After(b.ExpiresAt) {
		s.mu.Unlock()
		return nil, ErrNoClient
	}
	sess := s.newSessionLocked(DirIncoming, number, b.Addr, b.User)
	wait := make(chan *Message, 8)
	s.pendingInvite[sess.CallID] = wait
	s.mu.Unlock()

	adv := s.advertiseAddr()
	inv := NewRequest("INVITE", fmt.Sprintf("sip:%s@%s", b.User, b.Addr.IP.String()))
	inv.SetHeader("Via", "SIP/2.0/UDP "+adv+";branch="+branch())
	inv.SetHeader("From", fmt.Sprintf("<sip:%s@%s>;tag=%s", number, s.advertiseIP(), sess.serverTag))
	inv.SetHeader("To", fmt.Sprintf("<sip:%s@%s>", b.User, b.Addr.IP.String()))
	inv.SetHeader("Call-ID", sess.CallID)
	sess.cseq = 1
	inv.SetHeader("CSeq", "1 INVITE")
	inv.SetHeader("Contact", "<sip:gateway@"+adv+">")
	inv.SetHeader("Content-Type", "application/sdp")
	inv.Body = BuildSDP(s.advertiseIP(), sess.rtpPort)

	s.send(inv, b.Addr)
	log.Printf("sip: INVITE -> %s (call %s)", number, sess.ID)

	retransmit := time.NewTimer(500 * time.Millisecond)
	defer retransmit.Stop()
	provisional := false
	for {
		select {
		case <-ctx.Done():
			sess.Terminate("cancelled")
			return nil, ctx.Err()
		case resp := <-wait:
			switch {
			case resp.StatusCode == 100:
				provisional = true
			case resp.StatusCode == 180 || resp.StatusCode == 183:
				provisional = true
			case resp.StatusCode >= 200 && resp.StatusCode < 300:
				sess.mu.Lock()
				sess.clientTag = parseTag(resp.GetHeader("To"))
				if sdp, err := ParseSDP(resp.Body); err == nil {
					sess.remoteRTP = &net.UDPAddr{IP: net.ParseIP(sdp.IP), Port: sdp.Port}
				}
				sess.mu.Unlock()
				s.sendAck(sess, resp)
				if sess.setActive() && s.cb.OnAnswered != nil {
					go s.cb.OnAnswered(sess)
				}
				return sess, nil
			case resp.StatusCode >= 300:
				sess.Terminate(fmt.Sprintf("rejected %d", resp.StatusCode))
				return nil, fmt.Errorf("sip: INVITE rejected %d %s", resp.StatusCode, resp.Reason)
			}
		case <-retransmit.C:
			if !provisional {
				s.send(inv, b.Addr)
			}
			retransmit.Reset(2 * time.Second)
		}
	}
}

func (s *Server) sendAck(sess *Session, ok *Message) {
	ack := NewRequest("ACK", "sip:"+sess.clientURI)
	ack.SetHeader("Via", "SIP/2.0/UDP "+s.advertiseAddr()+";branch="+branch())
	ack.SetHeader("From", ok.GetHeader("From"))
	to := ok.GetHeader("To")
	if sess.clientTag != "" && !strings.Contains(to, "tag=") {
		to += ";tag=" + sess.clientTag
	}
	ack.SetHeader("To", to)
	ack.SetHeader("Call-ID", sess.CallID)
	ack.SetHeader("CSeq", fmt.Sprintf("%d ACK", sess.cseq))
	s.send(ack, sess.client)
}

// handleInvite: client -> server (outgoing call).
func (s *Server) handleInvite(req *Message, src *net.UDPAddr) {
	// Duplicate INVITE (client retransmit): resend last response, no new session.
	if existing := s.sessionByCallID(req.GetHeader("Call-ID")); existing != nil {
		existing.mu.Lock()
		st := existing.State
		ok200 := existing.ok200
		tag := existing.serverTag
		existing.mu.Unlock()
		switch st {
		case StateRinging:
			s.respond(req, src, 180, "Ringing", func(r *Message) {
				r.SetHeader("To", req.GetHeader("To")+";tag="+tag)
				r.SetHeader("Contact", "<sip:gateway@"+s.advertiseAddr()+">")
			})
		case StateActive:
			if ok200 != nil {
				s.send(ok200, src)
			}
		}
		return
	}
	number := sipUser(req.RequestURI)
	if number == "" {
		number = sipUser(req.GetHeader("To"))
	}
	sess := s.newSession(DirOutgoing, number, src, s.cfg.Username)
	// Adopt the client's Call-ID (newSession registered a generated one).
	s.mu.Lock()
	delete(s.byCallID, sess.CallID)
	sess.CallID = req.GetHeader("Call-ID")
	s.byCallID[sess.CallID] = sess
	s.mu.Unlock()
	sess.clientTag = parseTag(req.GetHeader("From"))
	sess.inviteReq = req
	if sdp, err := ParseSDP(req.Body); err == nil {
		sess.remoteRTP = &net.UDPAddr{IP: net.ParseIP(sdp.IP), Port: sdp.Port}
	}

	s.respond(req, src, 100, "Trying", nil)
	s.respond(req, src, 180, "Ringing", func(r *Message) {
		r.SetHeader("To", req.GetHeader("To")+";tag="+sess.serverTag)
		r.SetHeader("Contact", "<sip:gateway@"+s.advertiseAddr()+">")
	})
	log.Printf("sip: outgoing call to %s (session %s)", number, sess.ID)
	if s.cb.OnOutgoingCall != nil {
		go s.cb.OnOutgoingCall(sess)
	}
}

// Accept answers an outgoing call (controller calls this when the modem connects).
func (sess *Session) Accept() {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.State != StateRinging || sess.inviteReq == nil {
		return
	}
	s := sess.srv
	ok := NewResponse(200, "OK")
	ok.SetHeader("Via", sess.inviteReq.GetHeader("Via"))
	ok.SetHeader("From", sess.inviteReq.GetHeader("From"))
	ok.SetHeader("To", sess.inviteReq.GetHeader("To")+";tag="+sess.serverTag)
	ok.SetHeader("Call-ID", sess.CallID)
	ok.SetHeader("CSeq", sess.inviteReq.GetHeader("CSeq"))
	ok.SetHeader("Contact", "<sip:gateway@"+s.advertiseAddr()+">")
	ok.SetHeader("Content-Type", "application/sdp")
	ok.Body = BuildSDP(s.advertiseIP(), sess.rtpPort)
	sess.ok200 = ok
	s.send(ok, sess.client)
	sess.startRTPLocked()
	// Accept() is the outgoing-call twin of handleAck: the modem picked up, the
	// 200 OK goes to the phone, and the session becomes active. Without this the
	// controller never started the audio bridge, so calls placed *from* the
	// phone connected and carried nothing.
	sess.State = StateActive
	log.Printf("sip: 200 OK -> outgoing call %s", sess.ID)
	if s.cb.OnAnswered != nil {
		go s.cb.OnAnswered(sess)
	}
	// Note: 200 is retransmitted if the client re-sends INVITE (see dispatch).
}

func (s *Server) handleAck(req *Message) {
	sess := s.sessionByCallID(req.GetHeader("Call-ID"))
	if sess == nil {
		return
	}
	if sess.setActive() && s.cb.OnAnswered != nil {
		go s.cb.OnAnswered(sess)
	}
}

// Bye ends the call from the server side (modem hung up).
func (sess *Session) Bye() {
	sess.mu.Lock()
	if sess.State == StateEnded {
		sess.mu.Unlock()
		return
	}
	sess.cseq++
	cseq := sess.cseq
	client := sess.client
	callID := sess.CallID
	srv := sess.srv
	sess.mu.Unlock()

	bye := NewRequest("BYE", "sip:"+sess.clientURI)
	bye.SetHeader("Via", "SIP/2.0/UDP "+srv.advertiseAddr()+";branch="+branch())
	bye.SetHeader("From", fmt.Sprintf("<sip:gateway@%s>;tag=%s", srv.advertiseIP(), sess.serverTag))
	bye.SetHeader("To", fmt.Sprintf("<sip:%s>;tag=%s", sess.clientURI, sess.clientTag))
	bye.SetHeader("Call-ID", callID)
	bye.SetHeader("CSeq", fmt.Sprintf("%d BYE", cseq))
	srv.send(bye, client)
	sess.Terminate("bye-sent")
}

func (s *Server) handleBye(req *Message, src *net.UDPAddr) {
	s.respond(req, src, 200, "OK", nil)
	sess := s.sessionByCallID(req.GetHeader("Call-ID"))
	if sess != nil {
		sess.Terminate("client-bye")
	}
}

func (s *Server) handleCancel(req *Message, src *net.UDPAddr) {
	s.respond(req, src, 200, "OK", nil)
	sess := s.sessionByCallID(req.GetHeader("Call-ID"))
	if sess == nil {
		return
	}
	sess.mu.Lock()
	ringing := sess.State == StateRinging
	inviteReq := sess.inviteReq
	sess.mu.Unlock()
	if ringing && sess.Direction == DirOutgoing && inviteReq != nil {
		s.respond(inviteReq, src, 487, "Request Terminated", func(r *Message) {
			r.SetHeader("To", inviteReq.GetHeader("To")+";tag="+sess.serverTag)
		})
	}
	sess.Terminate("cancelled")
}

// --- MESSAGE (SMS) ---

func (s *Server) handleMessageReq(req *Message, src *net.UDPAddr) {
	to := sipUser(req.RequestURI)
	from := sipUser(req.GetHeader("From"))
	s.respond(req, src, 200, "OK", nil)
	log.Printf("sip: MESSAGE %s -> %s (%d bytes) body=%q", from, to, len(req.Body), req.Body)
	if s.cb.OnMessage != nil {
		go s.cb.OnMessage(from, to, req.Body)
	}
}

// SendMessageToClient delivers an incoming SMS to the phone via SIP MESSAGE.
func (s *Server) SendMessageToClient(from, body string) error {
	s.mu.Lock()
	b := s.binding
	s.mu.Unlock()
	if b == nil || time.Now().After(b.ExpiresAt) {
		return errors.New("sip: no registered client")
	}
	m := NewRequest("MESSAGE", fmt.Sprintf("sip:%s@%s", b.User, b.Addr.IP.String()))
	m.SetHeader("Via", "SIP/2.0/UDP "+s.advertiseAddr()+";branch="+branch())
	m.SetHeader("From", fmt.Sprintf("<sip:%s@gateway>;tag=%s", from, randHex(8)))
	m.SetHeader("To", fmt.Sprintf("<sip:%s@%s>", b.User, b.Addr.IP.String()))
	m.SetHeader("Call-ID", randHex(12)+"@gateway")
	m.SetHeader("CSeq", "1 MESSAGE")
	m.SetHeader("Content-Type", "text/plain; charset=utf-8")
	m.Body = body
	s.send(m, b.Addr)
	return nil
}

// --- session bookkeeping ---

func (s *Server) newSession(dir Direction, number string, client *net.UDPAddr, clientUser string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.newSessionLocked(dir, number, client, clientUser)
}

func (s *Server) newSessionLocked(dir Direction, number string, client *net.UDPAddr, clientUser string) *Session {
	sess := &Session{
		ID:        randHex(8),
		CallID:    randHex(12) + "@gateway",
		Direction: dir,
		Number:    number,
		State:     StateRinging,
		srv:       s,
		client:    client,
		clientURI: fmt.Sprintf("%s@%s", clientUser, client.IP.String()),
		serverTag: randHex(8),
		rtpPort:   s.pickRTPPortLocked(),
		done:      make(chan struct{}),
	}
	s.sessions[sess.ID] = sess
	s.byCallID[sess.CallID] = sess
	return sess
}

// pickRTPPortLocked returns the media port for a new session.
//
// It is deliberately the *same* port for every call. iOS only lets an app
// reach the local network while the app is in the foreground: a UDP socket
// created during a call (CallKit owns the screen by then) is denied silently —
// every send reports success and nobody ever receives anything. The phone
// therefore opens its media socket once, in the foreground, at connect time,
// and reuses it; a fixed port on this side is what makes that possible.
// Sessions are serialised anyway (one call at a time), and a port that really
// is taken falls through to the next one.
func (s *Server) pickRTPPortLocked() int {
	for port := s.cfg.RTPBase; port < s.cfg.RTPBase+40; port += 2 {
		if !s.rtpPortInUseLocked(port) {
			return port
		}
	}
	return s.cfg.RTPBase
}

func (s *Server) rtpPortInUseLocked(port int) bool {
	for _, sess := range s.sessions {
		if sess.rtpPort == port && sess.State != StateEnded {
			return true
		}
	}
	return false
}

func (s *Server) sessionByCallID(callID string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byCallID[callID]
}

// setActive transitions ringing->active and starts RTP. Returns true on transition.
func (sess *Session) setActive() bool {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	if sess.State != StateRinging {
		return false
	}
	sess.State = StateActive
	sess.startRTPLocked()
	return true
}

func (sess *Session) startRTPLocked() {
	if sess.rtpStarted {
		return
	}
	sess.rtpStarted = true
	if sess.srv.cfg.LoopbackRTP {
		rs, err := rtp.Listen(sess.rtpPort, true)
		if err != nil {
			log.Printf("sip: rtp listen :%d failed: %v", sess.rtpPort, err)
			return
		}
		sess.rtpSess = rs
		go rs.Run()
	}
	// Real-audio mode: the controller wires audio via OnSessionActive using
	// RTPPort()/RemoteRTP(). Loopback stays off.
}

// RTPPort is the local RTP port for this session.
func (sess *Session) RTPPort() int { return sess.rtpPort }

// RemoteRTP is the phone's RTP endpoint (from SDP).
func (sess *Session) RemoteRTP() *net.UDPAddr {
	sess.mu.Lock()
	defer sess.mu.Unlock()
	return sess.remoteRTP
}

// Terminate ends the session (idempotent) and fires OnEnded once.
func (sess *Session) Terminate(reason string) {
	sess.once.Do(func() {
		sess.mu.Lock()
		sess.State = StateEnded
		close(sess.done)
		if sess.rtpSess != nil {
			sess.rtpSess.Close()
		}
		sess.mu.Unlock()
		s := sess.srv
		s.mu.Lock()
		delete(s.sessions, sess.ID)
		delete(s.byCallID, sess.CallID)
		delete(s.pendingInvite, sess.CallID)
		s.mu.Unlock()
		log.Printf("sip: session %s ended (%s)", sess.ID, reason)
		if s.cb.OnEnded != nil {
			go s.cb.OnEnded(sess, reason)
		}
	})
}

// --- helpers ---

func randHex(n int) string {
	b := make([]byte, (n+1)/2)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)[:n]
}

func branch() string { return "z9hG4bK" + randHex(12) }

// sipUser extracts the user part from a sip: URI.
func sipUser(uri string) string {
	uri = strings.Trim(uri, "<> ")
	if i := strings.Index(uri, "sip:"); i >= 0 {
		uri = uri[i+4:]
	}
	if i := strings.Index(uri, "@"); i >= 0 {
		uri = uri[:i]
	}
	if i := strings.Index(uri, ";"); i >= 0 {
		uri = uri[:i]
	}
	return strings.TrimSpace(uri)
}

func parseTag(h string) string {
	for _, part := range strings.Split(h, ";") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "tag=") {
			return strings.TrimPrefix(part, "tag=")
		}
	}
	return ""
}

func parseContact(h string, src *net.UDPAddr) (host string, port int) {
	port = 5060
	// A Contact without an explicit port is common from clients that let the OS
	// pick an ephemeral local port (the iPhone app used to do exactly that).
	// Falling back to the port the REGISTER actually came from keeps incoming
	// INVITEs deliverable instead of sending them to a dead 5060.
	if src != nil && src.Port != 0 {
		port = src.Port
	}
	uri := h
	if i := strings.Index(uri, "<"); i >= 0 {
		if j := strings.Index(uri[i:], ">"); j >= 0 {
			uri = uri[i+1 : i+j]
		}
	}
	uri = strings.TrimSpace(uri)
	if strings.HasPrefix(uri, "sip:") {
		uri = uri[4:]
	}
	if i := strings.Index(uri, "@"); i >= 0 {
		uri = uri[i+1:]
	}
	if i := strings.Index(uri, ";"); i >= 0 {
		uri = uri[:i]
	}
	if h2, p, err := net.SplitHostPort(uri); err == nil {
		if h2 != "" {
			host = h2
		}
		if pn, err := strconv.Atoi(p); err == nil {
			port = pn
		}
	} else if uri != "" && uri != "*" {
		host = uri
	}
	if host == "" && src != nil {
		host = src.IP.String()
	}
	return host, port
}

func parseExpires(h string) time.Duration {
	if h == "" {
		return 3600 * time.Second
	}
	n, err := strconv.Atoi(strings.TrimSpace(h))
	if err != nil || n <= 0 {
		return 3600 * time.Second
	}
	return time.Duration(n) * time.Second
}

func localIP() string {
	var tailscale, lan string
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() || ipnet.IP.To4() == nil {
			continue
		}
		ip := ipnet.IP.String()
		// Tailscale uses 100.64.0.0/10 (CGNAT). Prefer it: works both on LAN
		// and remotely, so the phone never needs to switch server addresses.
		if strings.HasPrefix(ip, "100.") {
			tailscale = ip
		} else if lan == "" && !strings.HasPrefix(ip, "169.254.") {
			lan = ip
		}
	}
	if tailscale != "" {
		return tailscale
	}
	if lan != "" {
		return lan
	}
	return "127.0.0.1"
}
