package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Trigger sources.
const (
	TriggerMCP     = "mcp"     // poll a read-only integration tool for new items
	TriggerWebhook = "webhook" // the job's URL, filtered by Match
)

// Trigger makes a job fire on an event instead of the clock.
type Trigger struct {
	JobID      int64
	Source     string
	Server     string
	Tool       string
	Args       string // JSON
	EveryMin   int
	Match      string
	Seen       []string
	Primed     bool
	LastPollAt time.Time
	LastError  string
}

const triggerCols = "job_id, source, server, tool, args, every_min, match, seen, primed, last_poll_at, last_error"

func scanTrigger(r interface{ Scan(...any) error }) (*Trigger, error) {
	var t Trigger
	var seen string
	var last sql.NullInt64
	if err := r.Scan(&t.JobID, &t.Source, &t.Server, &t.Tool, &t.Args, &t.EveryMin, &t.Match, &seen, &t.Primed, &last, &t.LastError); err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(seen), &t.Seen)
	t.LastPollAt = timeOf(last)
	return &t, nil
}

func (s *Store) PutTrigger(ctx context.Context, t Trigger) error {
	if t.Args == "" {
		t.Args = "{}"
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO triggers (job_id, source, server, tool, args, every_min, match) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(job_id) DO UPDATE SET source = excluded.source, server = excluded.server, tool = excluded.tool, args = excluded.args,
		every_min = excluded.every_min, match = excluded.match`,
		t.JobID, t.Source, t.Server, t.Tool, t.Args, t.EveryMin, t.Match)
	return err
}

// TriggerFor returns a job's trigger, or ErrNotFound if it has none.
func (s *Store) TriggerFor(ctx context.Context, jobID int64) (*Trigger, error) {
	t, err := scanTrigger(s.db.QueryRowContext(ctx, "SELECT "+triggerCols+" FROM triggers WHERE job_id = ?", jobID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return t, err
}

// PollTriggers lists integration triggers of enabled jobs.
func (s *Store) PollTriggers(ctx context.Context) ([]Trigger, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT t.job_id, t.source, t.server, t.tool, t.args, t.every_min, t.match, t.seen, t.primed, t.last_poll_at, t.last_error
		FROM triggers t JOIN jobs j ON j.id = t.job_id WHERE t.source = 'mcp' AND j.enabled = 1 ORDER BY t.job_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Trigger
	for rows.Next() {
		t, err := scanTrigger(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// maxSeen bounds how many item keys a trigger remembers.
const maxSeen = 1000

// SavePoll records a poll: the keys now seen (newest last), and the error
// if it failed ("" for success).
func (s *Store) SavePoll(ctx context.Context, jobID int64, seen []string, errText string) error {
	if len(seen) > maxSeen {
		seen = seen[len(seen)-maxSeen:]
	}
	b, _ := json.Marshal(nonNilSlice(seen))
	_, err := s.db.ExecContext(ctx, "UPDATE triggers SET seen = ?, primed = 1, last_poll_at = ?, last_error = ? WHERE job_id = ?",
		string(b), s.unix(), errText, jobID)
	return err
}

// SavePollError records a failed poll, keeping what was seen.
func (s *Store) SavePollError(ctx context.Context, jobID int64, errText string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE triggers SET last_poll_at = ?, last_error = ? WHERE job_id = ?", s.unix(), errText, jobID)
	return err
}
