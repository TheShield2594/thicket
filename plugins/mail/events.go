package main

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Event is one line of /v1/events. Bodies are never in events.
type Event struct {
	Seq       int64    `json:"seq"`
	Sprig     string   `json:"sprig"`
	Kind      string   `json:"kind"`
	At        string   `json:"at"`
	From      string   `json:"from,omitempty"`
	To        []string `json:"to,omitempty"`
	Cc        []string `json:"cc,omitempty"`
	Bcc       []string `json:"bcc,omitempty"`
	Subject   string   `json:"subject,omitempty"`
	Size      int64    `json:"size,omitempty"`
	MessageID string   `json:"message_id,omitempty"`
	OTP       *bool    `json:"otp,omitempty"`
	Approved  *bool    `json:"approved,omitempty"`
	ID        string   `json:"id,omitempty"`
	DraftID   string   `json:"draft_id,omitempty"`
	Reason    string   `json:"reason,omitempty"`
	Detail    string   `json:"detail,omitempty"`
}

// eventLog keeps the last events in a ring, numbered from 1. epoch changes
// on every start so core knows to begin again from after=0.
type eventLog struct {
	epoch string
	max   int

	mu     sync.Mutex
	events []Event
	last   int64
	wake   chan struct{} // closed and replaced on every add
}

func newEventLog(max int) *eventLog {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return &eventLog{epoch: hex.EncodeToString(b), max: max, wake: make(chan struct{})}
}

func (l *eventLog) add(e Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.last++
	e.Seq = l.last
	if e.At == "" {
		e.At = time.Now().UTC().Format(time.RFC3339)
	}
	l.events = append(l.events, e)
	if len(l.events) > l.max {
		l.events = append([]Event(nil), l.events[len(l.events)-l.max:]...)
	}
	close(l.wake)
	l.wake = make(chan struct{})
}

// after returns the events after seq, the seq to ask from next, and a
// channel closed when there is something newer.
func (l *eventLog) after(seq int64) ([]Event, int64, <-chan struct{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := []Event{}
	for _, e := range l.events {
		if e.Seq > seq {
			out = append(out, e)
		}
	}
	next := seq
	if len(out) > 0 {
		next = out[len(out)-1].Seq
	} else if seq > l.last {
		next = l.last // a seq from before a restart
	}
	return out, next, l.wake
}

func (s *server) getEvents(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	wait, _ := strconv.Atoi(r.URL.Query().Get("wait"))
	wait = max(0, min(wait, 30))
	timeout := time.After(time.Duration(wait) * time.Second)
	for {
		events, next, wake := s.events.after(after)
		if len(events) > 0 || wait == 0 {
			writeJSON(w, http.StatusOK, map[string]any{"epoch": s.events.epoch, "next": next, "events": events})
			return
		}
		select {
		case <-wake:
		case <-timeout:
			wait = 0
		case <-r.Context().Done():
			return
		}
	}
}
