// Package history persists call and SMS records to a JSON file on disk so
// they survive gateway restarts. When the phone connects it pulls the full
// history (or a delta since a cursor) via GET /api/v1/history.
package history

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// CallRecord is one incoming or outgoing call.
type CallRecord struct {
	ID        string `json:"id"`
	Direction string `json:"direction"` // "in" | "out"
	Number    string `json:"number"`
	Status    string `json:"status"` // "answered" | "missed" | "rejected" | "failed"
	StartedAt string `json:"started_at"`
	EndedAt   string `json:"ended_at,omitempty"`
	Duration  int    `json:"duration"` // seconds, 0 for missed
}

// SMSRecord is one sent or received SMS.
type SMSRecord struct {
	ID        string `json:"id"`
	Direction string `json:"direction"` // "in" | "out"
	Peer      string `json:"peer"`
	Body      string `json:"body"`
	At        string `json:"at"`
	Status    string `json:"status"` // "sent" | "failed" | "received"
}

// Store holds all history in memory and flushes to disk on every mutation.
type Store struct {
	mu    sync.RWMutex
	calls []CallRecord
	sms   []SMSRecord
	path  string
}

type storeJSON struct {
	Calls []CallRecord `json:"calls"`
	SMS   []SMSRecord  `json:"sms"`
}

// NewStore loads (or creates) a history file at the given path.
func NewStore(dir string) *Store {
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Printf("history: mkdir %s: %v", dir, err)
	}
	s := &Store{path: filepath.Join(dir, "history.json")}
	s.load()
	return s
}

func (s *Store) load() {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return // fresh start
	}
	var j storeJSON
	if err := json.Unmarshal(data, &j); err != nil {
		log.Printf("history: parse %s: %v", s.path, err)
		return
	}
	s.calls = j.Calls
	s.sms = j.SMS
}

func (s *Store) flush() {
	j := storeJSON{Calls: s.calls, SMS: s.sms}
	if j.Calls == nil {
		j.Calls = []CallRecord{}
	}
	if j.SMS == nil {
		j.SMS = []SMSRecord{}
	}
	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		log.Printf("history: marshal: %v", err)
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		log.Printf("history: write %s: %v", tmp, err)
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		log.Printf("history: rename: %v", err)
	}
}

func newID() string {
	return time.Now().Format("20060102150405") + "-" +
		time.Now().Format(".000000")[1:] // microsecond uniqueness
}

// AddCall records a new call and returns its ID.
func (s *Store) AddCall(dir, number, status string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := CallRecord{
		ID:        newID(),
		Direction: dir,
		Number:    number,
		Status:    status,
		StartedAt: time.Now().Format(time.RFC3339),
	}
	s.calls = append(s.calls, r)
	s.flush()
	return r.ID
}

// EndCall sets the end time and duration.
func (s *Store) EndCall(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.calls) - 1; i >= 0; i-- {
		if s.calls[i].ID == id {
			now := time.Now()
			s.calls[i].EndedAt = now.Format(time.RFC3339)
			if started, err := time.Parse(time.RFC3339, s.calls[i].StartedAt); err == nil {
				s.calls[i].Duration = int(now.Sub(started).Seconds())
			}
			break
		}
	}
	s.flush()
}

// UpdateCallStatus marks a call as answered/missed/etc.
func (s *Store) UpdateCallStatus(id, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.calls) - 1; i >= 0; i-- {
		if s.calls[i].ID == id {
			s.calls[i].Status = status
			break
		}
	}
	s.flush()
}

// AddSMS records an SMS.
func (s *Store) AddSMS(dir, peer, body, status string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := SMSRecord{
		ID:        newID(),
		Direction: dir,
		Peer:      peer,
		Body:      body,
		At:        time.Now().Format(time.RFC3339),
		Status:    status,
	}
	s.sms = append(s.sms, r)
	s.flush()
	return r.ID
}

// Calls returns all call records, newest last.
func (s *Store) Calls() []CallRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]CallRecord, len(s.calls))
	copy(out, s.calls)
	return out
}

// SMS returns all SMS records, newest last.
func (s *Store) SMS() []SMSRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]SMSRecord, len(s.sms))
	copy(out, s.sms)
	return out
}

// CallsSince returns calls with ID > afterID.
func (s *Store) CallsSince(afterID string) []CallRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if afterID == "" {
		return s.Calls()
	}
	idx := -1
	for i, c := range s.calls {
		if c.ID == afterID {
			idx = i
			break
		}
	}
	if idx < 0 || idx+1 >= len(s.calls) {
		return nil
	}
	out := make([]CallRecord, len(s.calls)-idx-1)
	copy(out, s.calls[idx+1:])
	return out
}

// SMSSince returns SMS with ID > afterID.
func (s *Store) SMSSince(afterID string) []SMSRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if afterID == "" {
		return s.SMS()
	}
	idx := -1
	for i, m := range s.sms {
		if m.ID == afterID {
			idx = i
			break
		}
	}
	if idx < 0 || idx+1 >= len(s.sms) {
		return nil
	}
	out := make([]SMSRecord, len(s.sms)-idx-1)
	copy(out, s.sms[idx+1:])
	return out
}
