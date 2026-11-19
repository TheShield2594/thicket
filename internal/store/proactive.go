package store

import (
	"context"
	"time"
)

// --- proactive messages: the daily budget and held messages ---

// Why a held message is waiting.
const (
	HeldQuiet  = "quiet"  // the sprig's quiet hours
	HeldBudget = "budget" // the sprig's daily budget for this person was used up
)

// HeldMessage is a finished message a sprig started itself, waiting to
// be delivered.
type HeldMessage struct {
	ID        int64
	UserID    int64
	Character string
	Ref       string // job:ID
	Title     string // what it is, e.g. Job “Morning digest”
	Reason    string // HeldQuiet | HeldBudget
	Text      string
	CreatedAt time.Time
}

// LocalDayStart is midnight of t's day in loc.
func LocalDayStart(t time.Time, loc *time.Location) time.Time {
	l := t.In(loc)
	return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, loc)
}

// RecordProactive counts a proactive message a sprig delivered to a
// person at at (the caller's clock, so the count and the gate agree).
func (s *Store) RecordProactive(ctx context.Context, userID int64, character string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, "INSERT INTO proactive_sent (user_id, character, sent_at) VALUES (?, ?, ?)", userID, character, at.Unix())
	return err
}

// ProactiveSentSince counts the proactive messages a sprig delivered to a
// person since t.
func (s *Store) ProactiveSentSince(ctx context.Context, userID int64, character string, since time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM proactive_sent WHERE user_id = ? AND character = ? AND sent_at >= ?",
		userID, character, since.Unix()).Scan(&n)
	return n, err
}

// PruneProactive forgets counted messages from before t.
func (s *Store) PruneProactive(ctx context.Context, before time.Time) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM proactive_sent WHERE sent_at < ?", before.Unix())
	return err
}

// HoldMessage stores a message to deliver later. CreatedAt must be set.
func (s *Store) HoldMessage(ctx context.Context, h HeldMessage) (int64, error) {
	res, err := s.db.ExecContext(ctx, "INSERT INTO held_messages (user_id, character, ref, title, reason, text, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
		h.UserID, h.Character, h.Ref, s.clean(h.Title), h.Reason, s.clean(h.Text), h.CreatedAt.Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// HeldMessages lists held messages, oldest first: every one when userID
// is 0, else one person's (and one sprig's, when character isn't "").
func (s *Store) HeldMessages(ctx context.Context, userID int64, character string) ([]HeldMessage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, user_id, character, ref, title, reason, text, created_at FROM held_messages
		WHERE (? = 0 OR user_id = ?) AND (? = '' OR character = ?) ORDER BY id`, userID, userID, character, character)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HeldMessage
	for rows.Next() {
		var h HeldMessage
		var at int64
		if err := rows.Scan(&h.ID, &h.UserID, &h.Character, &h.Ref, &h.Title, &h.Reason, &h.Text, &at); err != nil {
			return nil, err
		}
		h.CreatedAt = time.Unix(at, 0)
		out = append(out, h)
	}
	return out, rows.Err()
}

// SetHeldReason records why a held message is still waiting.
func (s *Store) SetHeldReason(ctx context.Context, id int64, reason string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE held_messages SET reason = ? WHERE id = ?", reason, id)
	return err
}

// TakeHeld removes a held message, reporting whether it was still there:
// whoever takes it delivers (or drops) it, exactly once.
func (s *Store) TakeHeld(ctx context.Context, id int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM held_messages WHERE id = ?", id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}
