package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// The trail records every permission change and every sprig-created
// event, with what it takes to undo each one. Unlike the activity log it
// is never pruned.

// Trail kinds.
const (
	TrailLevel  = "level"  // a tool's permission level changed
	TrailGrant  = "grant"  // a sprig's tool scopes changed
	TrailEvent  = "event"  // a sprig put something on the calendar
	TrailRole   = "role"   // someone's household role changed
	TrailSafety = "safety" // a safety alert went to a kid's parents
	TrailUndo   = "undo"   // something above was undone
	TrailUpdate = "update" // Thicket was updated to a new version
	TrailHeld   = "held"   // an action after untrusted content went to the inbox instead
)

type TrailEntry struct {
	ID        int64     `json:"id"`
	At        time.Time `json:"at"`
	UserID    int64     `json:"user_id,omitempty"`
	Character string    `json:"character,omitempty"`
	Kind      string    `json:"kind"`
	Summary   string    `json:"summary"`
	Detail    string    `json:"detail,omitempty"`
	// Undo is what reverses the change (see agent/trail.go); empty when
	// there's nothing to undo.
	Undo     json.RawMessage `json:"-"`
	Undoable bool            `json:"undoable"`
	UndoneAt time.Time       `json:"undone_at,omitzero"`
}

func (s *Store) AddTrail(ctx context.Context, t TrailEntry) error {
	_, err := s.AddTrailID(ctx, t)
	return err
}

// AddTrailID records an entry and returns its id.
func (s *Store) AddTrailID(ctx context.Context, t TrailEntry) (int64, error) {
	res, err := s.db.ExecContext(ctx, "INSERT INTO trail (at, user_id, character, kind, summary, detail, undo) VALUES (?, ?, ?, ?, ?, ?, ?)",
		s.unix(), nullInt(t.UserID), t.Character, t.Kind, s.clean(t.Summary), s.clean(t.Detail), string(t.Undo))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

const trailCols = "id, at, user_id, character, kind, summary, detail, undo, undone_at"

func scanTrail(r interface{ Scan(...any) error }) (*TrailEntry, error) {
	var t TrailEntry
	var at int64
	var uid, undone sql.NullInt64
	var undo string
	if err := r.Scan(&t.ID, &at, &uid, &t.Character, &t.Kind, &t.Summary, &t.Detail, &undo, &undone); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	t.At, t.UserID, t.UndoneAt = time.Unix(at, 0), uid.Int64, timeOf(undone)
	if undo != "" {
		t.Undo = json.RawMessage(undo)
	}
	t.Undoable = undo != "" && !undone.Valid
	return &t, nil
}

// Trail returns the newest entries, optionally only one user's (0 = all).
func (s *Store) Trail(ctx context.Context, userID int64, limit int) ([]TrailEntry, error) {
	q, args := "SELECT "+trailCols+" FROM trail", []any{}
	if userID != 0 {
		q += " WHERE user_id = ?"
		args = append(args, userID)
	}
	rows, err := s.db.QueryContext(ctx, q+" ORDER BY id DESC LIMIT ?", append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TrailEntry
	for rows.Next() {
		t, err := scanTrail(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

func (s *Store) TrailByID(ctx context.Context, id int64) (*TrailEntry, error) {
	return scanTrail(s.db.QueryRowContext(ctx, "SELECT "+trailCols+" FROM trail WHERE id = ?", id))
}

// MarkTrailUndone records an undo, once: false if it was already undone.
func (s *Store) MarkTrailUndone(ctx context.Context, id, by int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, "UPDATE trail SET undone_at = ?, undone_by = ? WHERE id = ? AND undone_at IS NULL AND undo != ''", s.unix(), nullInt(by), id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// ClearTrailUndone takes back a claimed undo that failed.
func (s *Store) ClearTrailUndone(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, "UPDATE trail SET undone_at = NULL, undone_by = NULL WHERE id = ?", id)
	return err
}
