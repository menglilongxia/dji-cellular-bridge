// Package sms stores messages and moves them between the modem and the phone.
package sms

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/dji-cellular-bridge/mac-gateway/internal/modem"
)

// Message is one SMS, either direction.
type Message struct {
	ID        string `json:"id"`
	Direction string `json:"direction"` // "in" | "out"
	Peer      string `json:"peer"`
	Body      string `json:"body"`
	At        string `json:"at"`
	Status    string `json:"status"` // "sent" | "failed" | "received"
}

// Engine keeps the message store and talks to the modem.
type Engine struct {
	mu   sync.Mutex
	msgs []Message
	drv  modem.Driver

	// OnIncoming fires for each received SMS (controller forwards it).
	OnIncoming func(m Message)
}

func NewEngine(drv modem.Driver) *Engine { return &Engine{drv: drv} }

func newID() string { return fmt.Sprintf("%d", time.Now().UnixNano()) }

// Send transmits an SMS via the modem and stores it.
func (e *Engine) Send(ctx context.Context, to, body string) (*Message, error) {
	if err := e.drv.SendSMS(ctx, to, body); err != nil {
		return nil, err
	}
	m := Message{ID: newID(), Direction: "out", Peer: to, Body: body,
		At: time.Now().Format(time.RFC3339), Status: "sent"}
	e.mu.Lock()
	e.msgs = append(e.msgs, m)
	e.mu.Unlock()
	return &m, nil
}

// HandleModemSMS reads a +CMTI-indicated SMS, stores it, deletes it from
// the module, and notifies the controller.
func (e *Engine) HandleModemSMS(ctx context.Context, index int) (*Message, error) {
	from, text, err := e.drv.ReadSMS(ctx, index)
	if err != nil {
		return nil, err
	}
	_ = e.drv.DeleteSMS(ctx, index) // best effort
	m := Message{ID: newID(), Direction: "in", Peer: from, Body: text,
		At: time.Now().Format(time.RFC3339), Status: "received"}
	e.mu.Lock()
	e.msgs = append(e.msgs, m)
	e.mu.Unlock()
	if e.OnIncoming != nil {
		e.OnIncoming(m)
	}
	return &m, nil
}

// List returns messages, newest last.
func (e *Engine) List() []Message {
	e.mu.Lock()
	defer e.mu.Unlock()
	cp := make([]Message, len(e.msgs))
	copy(cp, e.msgs)
	return cp
}
