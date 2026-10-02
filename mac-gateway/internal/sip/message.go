// Package sip implements a minimal SIP stack (RFC 3261 subset) sufficient
// for one gateway and one phone client: REGISTER with digest auth,
// INVITE/BYE/ACK/CANCEL, MESSAGE, OPTIONS. Both the Go gateway and the
// Swift iOS app implement this same profile (see docs/PROTOCOL.md).
package sip

import (
	"fmt"
	"strconv"
	"strings"
)

type header struct {
	name   string // as first seen (display case)
	values []string
}

// Message is a parsed SIP message.
type Message struct {
	IsRequest  bool
	Method     string // requests
	RequestURI string // requests
	StatusCode int    // responses
	Reason     string // responses

	order   []*header
	headers map[string]*header // key = lower-cased name
	Body    string
}

func newMessage() *Message { return &Message{headers: make(map[string]*header)} }

// ParseMessage parses one SIP message from raw bytes.
func ParseMessage(data []byte) (*Message, error) {
	text := string(data)
	head, body, _ := strings.Cut(text, "\r\n\r\n")
	lines := strings.Split(head, "\r\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) == "" {
		return nil, fmt.Errorf("sip: empty message")
	}
	m := newMessage()
	start := lines[0]
	if strings.HasPrefix(start, "SIP/2.0") {
		m.IsRequest = false
		parts := strings.SplitN(start, " ", 3)
		if len(parts) < 2 {
			return nil, fmt.Errorf("sip: bad status line %q", start)
		}
		code, err := strconv.Atoi(parts[1])
		if err != nil {
			return nil, fmt.Errorf("sip: bad status code %q", start)
		}
		m.StatusCode = code
		if len(parts) == 3 {
			m.Reason = parts[2]
		}
	} else {
		m.IsRequest = true
		parts := strings.SplitN(start, " ", 3)
		if len(parts) != 3 {
			return nil, fmt.Errorf("sip: bad request line %q", start)
		}
		m.Method, m.RequestURI = parts[0], parts[1]
	}
	// Unfold continuation lines (rare, but legal).
	var last *header
	for _, ln := range lines[1:] {
		if ln == "" {
			continue
		}
		if (ln[0] == ' ' || ln[0] == '\t') && last != nil {
			last.values[len(last.values)-1] += " " + strings.TrimSpace(ln)
			continue
		}
		name, value, ok := strings.Cut(ln, ":")
		if !ok {
			continue
		}
		last = m.addHeader(strings.TrimSpace(name), strings.TrimSpace(value))
	}
	m.Body = body
	return m, nil
}

func (m *Message) addHeader(name, value string) *header {
	key := strings.ToLower(name)
	h, ok := m.headers[key]
	if !ok {
		h = &header{name: name}
		m.headers[key] = h
		m.order = append(m.order, h)
	}
	h.values = append(h.values, value)
	return h
}

// AddHeader appends a header value.
func (m *Message) AddHeader(name, value string) { m.addHeader(name, value) }

// SetHeader replaces a header.
func (m *Message) SetHeader(name, value string) {
	key := strings.ToLower(name)
	if h, ok := m.headers[key]; ok {
		h.values = []string{value}
		return
	}
	m.addHeader(name, value)
}

// GetHeader returns the first value of a header (case-insensitive).
func (m *Message) GetHeader(name string) string {
	if h, ok := m.headers[strings.ToLower(name)]; ok && len(h.values) > 0 {
		return h.values[0]
	}
	return ""
}

// NewRequest creates an empty request.
func NewRequest(method, uri string) *Message {
	m := newMessage()
	m.IsRequest = true
	m.Method = method
	m.RequestURI = uri
	return m
}

// NewResponse creates an empty response.
func NewResponse(status int, reason string) *Message {
	m := newMessage()
	m.IsRequest = false
	m.StatusCode = status
	m.Reason = reason
	return m
}

// Serialize renders the message. Content-Length is set automatically.
func (m *Message) Serialize() string {
	var b strings.Builder
	if m.IsRequest {
		fmt.Fprintf(&b, "%s %s SIP/2.0\r\n", m.Method, m.RequestURI)
	} else {
		fmt.Fprintf(&b, "SIP/2.0 %d %s\r\n", m.StatusCode, m.Reason)
	}
	m.SetHeader("Content-Length", strconv.Itoa(len(m.Body)))
	for _, h := range m.order {
		for _, v := range h.values {
			fmt.Fprintf(&b, "%s: %s\r\n", h.name, v)
		}
	}
	b.WriteString("\r\n")
	b.WriteString(m.Body)
	return b.String()
}
