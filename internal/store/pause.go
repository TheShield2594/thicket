package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// PauseAll is the scope of a pause of everything.
const PauseAll = "*"

// Pause is one pause in force: of everything (Scope "*") or of one sprig.
type Pause struct {
	Scope    string    `json:"scope"`
	PausedBy int64     `json:"paused_by,omitempty"`
	Reason   string    `json:"reason,omitempty"`
	PausedAt time.Time `json:"paused_at"`
}

// SetPause pauses a scope. It reports whether it wasn't paused already
// (an existing pause keeps its time and author).
func (s *Store) SetPause(ctx context.Context, scope string, by int64, reason string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO pauses (scope, paused_by, reason, paused_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(scope) DO NOTHING`, scope, nullID(by), reason, s.unix())
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// ClearPause resumes a scope. It reports whether it was paused.
func (s *Store) ClearPause(ctx context.Context, scope string) (bool, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM pauses WHERE scope = ?", scope)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// Pauses lists the pauses in force, everything first.
func (s *Store) Pauses(ctx context.Context) ([]Pause, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT scope, COALESCE(paused_by, 0), reason, paused_at FROM pauses
		ORDER BY scope <> '*', scope`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Pause
	for rows.Next() {
		var p Pause
		var at int64
		if err := rows.Scan(&p.Scope, &p.PausedBy, &p.Reason, &at); err != nil {
			return nil, err
		}
		p.PausedAt = time.Unix(at, 0)
		out = append(out, p)
	}
	return out, rows.Err()
}

// PausedFor returns the pause that stops a sprig: everything, or the
// sprig itself (character "" checks only everything). ok is false when
// neither is paused.
func (s *Store) PausedFor(ctx context.Context, character string) (Pause, bool, error) {
	var p Pause
	var at int64
	err := s.db.QueryRowContext(ctx, `SELECT scope, COALESCE(paused_by, 0), reason, paused_at FROM pauses
		WHERE scope = '*' OR scope = ? ORDER BY scope <> '*' LIMIT 1`, character).Scan(&p.Scope, &p.PausedBy, &p.Reason, &at)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Pause{}, false, nil
		}
		return Pause{}, false, err
	}
	p.PausedAt = time.Unix(at, 0)
	return p, true, nil
}

// PausedExactly reports whether this scope itself is paused ("*", or one
// sprig regardless of a pause of everything).
func (s *Store) PausedExactly(ctx context.Context, scope string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM pauses WHERE scope = ?", scope).Scan(&n)
	return n > 0, err
}
