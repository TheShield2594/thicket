package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// sendLog remembers when each sprig sent mail, for the daily limit (a
// rolling 24 hours). It is the plugin's only state on disk: a small JSON
// file in the state dir, written atomically. No credentials, no addresses.
type sendLog struct {
	path string

	mu    sync.Mutex
	sends map[string][]time.Time
}

const sendWindow = 24 * time.Hour

func openSendLog(dir string) (*sendLog, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("state dir: %w", err)
	}
	l := &sendLog{path: filepath.Join(dir, "sends.json"), sends: map[string][]time.Time{}}
	b, err := os.ReadFile(l.path)
	if errors.Is(err, os.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return nil, fmt.Errorf("state file: %w", err)
	}
	var st struct {
		Sends map[string][]time.Time `json:"sends"`
	}
	if err := json.Unmarshal(b, &st); err != nil {
		// A broken file must not stop mail; start counting afresh.
		log.Printf("mail: ignoring unreadable %s: %v", l.path, err)
		return l, nil
	}
	if st.Sends != nil {
		l.sends = st.Sends
	}
	return l, nil
}

// recent drops sends older than the window (call with mu held).
func (l *sendLog) recent(sprig string, now time.Time) []time.Time {
	var keep []time.Time
	for _, t := range l.sends[sprig] {
		if now.Sub(t) < sendWindow {
			keep = append(keep, t)
		}
	}
	if keep == nil {
		delete(l.sends, sprig)
	} else {
		l.sends[sprig] = keep
	}
	return keep
}

// errLimit is reserve's answer when the sprig is at its daily limit.
var errLimit = errors.New("daily send limit reached")

// reserve counts one send for the sprig if it is under its limit (or the
// send is approved: approved sends count but are never refused). It
// returns a release function to undo the count if sending then fails.
//
// The count must survive a restart, or a restart would reset the limit.
// So when the count can't be written to disk, an unapproved send is
// refused (the reservation rolled back) with that error; an approved one
// still goes, since a person said so and approved sends are never refused.
func (l *sendLog) reserve(sprig string, limit int, approved bool, now time.Time) (func(), error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !approved && len(l.recent(sprig, now)) >= limit {
		return func() {}, errLimit
	}
	l.recent(sprig, now)
	l.sends[sprig] = append(l.sends[sprig], now)
	if err := l.save(); err != nil && !approved {
		l.drop(sprig, now)
		return func() {}, fmt.Errorf("the daily send count couldn't be saved (%v)", err)
	}
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.drop(sprig, now)
		_ = l.save() // logged; a send that didn't happen counting once is harmless
	}, nil
}

// drop removes one send at exactly t (call with mu held).
func (l *sendLog) drop(sprig string, t time.Time) {
	list := l.sends[sprig]
	for i := len(list) - 1; i >= 0; i-- {
		if list[i].Equal(t) {
			l.sends[sprig] = append(list[:i:i], list[i+1:]...)
			return
		}
	}
}

// count is how many sends the sprig made in the window.
func (l *sendLog) count(sprig string, now time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.recent(sprig, now))
}

// save writes the file atomically: a temp file, synced, renamed over the
// old one (call with mu held). Failures are logged and returned.
func (l *sendLog) save() error {
	b, _ := json.Marshal(map[string]any{"sends": l.sends})
	tmp, err := os.CreateTemp(filepath.Dir(l.path), ".sends-*.json")
	if err != nil {
		log.Printf("mail: saving send counts: %v", err)
		return err
	}
	_, err = tmp.Write(b)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), l.path)
	}
	if err != nil {
		os.Remove(tmp.Name())
		log.Printf("mail: saving send counts: %v", err)
	}
	return err
}
