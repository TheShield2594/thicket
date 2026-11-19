package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// --- the weekly digest (agent/digest.go) ---

// How a person wants the weekly digest.
const (
	DigestAlways    = "always"     // every week; a quiet week gets one line
	DigestSkipEmpty = "skip_empty" // only weeks with something to say
	DigestOff       = "off"        // never
)

// ValidDigestMode reports whether mode is one a person can choose.
func ValidDigestMode(mode string) bool {
	return mode == DigestAlways || mode == DigestSkipEmpty || mode == DigestOff
}

// DigestMode is how a person wants the weekly digest (DigestAlways if
// they never said).
func (s *Store) DigestMode(ctx context.Context, userID int64) (string, error) {
	var mode string
	err := s.db.QueryRowContext(ctx, "SELECT mode FROM digest_prefs WHERE user_id = ?", userID).Scan(&mode)
	if errors.Is(err, sql.ErrNoRows) {
		return DigestAlways, nil
	}
	return mode, err
}

// SetDigestMode stores how a person wants the weekly digest.
func (s *Store) SetDigestMode(ctx context.Context, userID int64, mode string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO digest_prefs (user_id, mode, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (user_id) DO UPDATE SET mode = excluded.mode, updated_at = excluded.updated_at`, userID, mode, s.unix())
	return err
}

// TrailBetween lists sprig trail entries (character not empty) recorded
// in [from, to), oldest first.
func (s *Store) TrailBetween(ctx context.Context, from, to time.Time) ([]TrailEntry, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+trailCols+" FROM trail WHERE character != '' AND at >= ? AND at < ? ORDER BY id LIMIT 5000",
		from.Unix(), to.Unix())
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

// SpendBetween is a sprig's model spend in [from, to), for everyone it
// worked for.
func (s *Store) SpendBetween(ctx context.Context, character string, from, to time.Time) (float64, error) {
	var v sql.NullFloat64
	err := s.db.QueryRowContext(ctx, "SELECT SUM(cost_usd) FROM usage WHERE character = ? AND at >= ? AND at < ?",
		character, from.Unix(), to.Unix()).Scan(&v)
	return v.Float64, err
}

// RepliesBetween counts the replies a sprig wrote in conversations of
// the given people in [from, to).
func (s *Store) RepliesBetween(ctx context.Context, character string, users []int64, from, to time.Time) (int, error) {
	if len(users) == 0 {
		return 0, nil
	}
	args := []any{character, from.Unix(), to.Unix()}
	for _, u := range users {
		args = append(args, u)
	}
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages m JOIN conversations c ON c.id = m.conversation_id
		WHERE m.role = 'assistant' AND m.character = ? AND m.created_at >= ? AND m.created_at < ?
		AND c.user_id IN (`+strings.Repeat("?,", len(users)-1)+`?)`, args...).Scan(&n)
	return n, err
}
