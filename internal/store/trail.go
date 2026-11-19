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
	TrailLevel   = "level"   // a tool's permission level changed
	TrailGrant   = "grant"   // a sprig's tool scopes changed
	TrailEvent   = "event"   // a sprig put something on the calendar
	TrailRole    = "role"    // someone's household role changed
	TrailSafety  = "safety"  // a safety alert went to a kid's parents
	TrailUndo    = "undo"    // something above was undone
	TrailUpdate  = "update"  // Thicket was updated to a new version
	TrailHeld    = "held"    // an action after untrusted content went to the inbox instead
	TrailAccess  = "access"  // a sprig's exec opt-in or granted paths changed
	TrailRefused = "refused" // someone tried to change a sprig that isn't theirs to change
	TrailPause   = "pause"   // everything, or one sprig, was paused or resumed
	TrailCap     = "cap"     // a sprig's own spend cap changed
	TrailAction  = "action"  // a sprig did something at act level (an integration, a job)
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

// AddTrailAcks records the chat app's ids for the line that said what a
// sprig did, so "undo" in reply to it finds the entry.
func (s *Store) AddTrailAcks(ctx context.Context, chatRef string, trailID, userID int64, platformIDs ...string) error {
	for _, pid := range platformIDs {
		if pid == "" {
			continue
		}
		if _, err := s.db.ExecContext(ctx, "INSERT OR REPLACE INTO trail_acks (chat_ref, platform_id, trail_id, user_id, created_at) VALUES (?, ?, ?, ?, ?)",
			chatRef, pid, trailID, userID, s.unix()); err != nil {
			return err
		}
	}
	return nil
}

// TrailAck is the trail entry a line in a chat said, and whose it is.
func (s *Store) TrailAck(ctx context.Context, chatRef, platformID string) (trailID, userID int64, err error) {
	err = s.db.QueryRowContext(ctx, "SELECT trail_id, user_id FROM trail_acks WHERE chat_ref = ? AND platform_id = ?", chatRef, platformID).
		Scan(&trailID, &userID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, ErrNotFound
	}
	return trailID, userID, err
}

// LatestTrailAck is the newest action said to this person in this chat
// since t that can still be undone.
func (s *Store) LatestTrailAck(ctx context.Context, chatRef string, userID int64, since time.Time) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT a.trail_id FROM trail_acks a JOIN trail t ON t.id = a.trail_id
		WHERE a.chat_ref = ? AND a.user_id = ? AND a.created_at >= ? AND t.undone_at IS NULL AND t.undo != ''
		ORDER BY a.created_at DESC, a.trail_id DESC LIMIT 1`, chatRef, userID, since.Unix()).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return id, err
}

// TrailOfKind returns the newest entries of one kind about one sprig that
// haven't been undone.
func (s *Store) TrailOfKind(ctx context.Context, kind, character string, limit int) ([]TrailEntry, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+trailCols+" FROM trail WHERE kind = ? AND character = ? AND undone_at IS NULL ORDER BY id DESC LIMIT ?",
		kind, character, limit)
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
