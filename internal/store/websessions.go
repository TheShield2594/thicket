package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// AddWebSession stores a login link or session (by token hash).
func (s *Store) AddWebSession(ctx context.Context, hash string, userID int64, kind string, ttl time.Duration) error {
	now := s.unix()
	// Clear expired rows while we're here.
	if _, err := s.db.ExecContext(ctx, "DELETE FROM web_sessions WHERE expires_at < ?", now); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, "INSERT INTO web_sessions (hash, user_id, kind, expires_at, created_at) VALUES (?, ?, ?, ?, ?)",
		hash, userID, kind, now+int64(ttl.Seconds()), now)
	return err
}

// WebSessionUser returns the user of a live session or login link.
func (s *Store) WebSessionUser(ctx context.Context, hash, kind string) (int64, error) {
	var uid int64
	err := s.db.QueryRowContext(ctx, "SELECT user_id FROM web_sessions WHERE hash = ? AND kind = ? AND expires_at >= ?", hash, kind, s.unix()).Scan(&uid)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return uid, err
}

// UseWebLogin consumes a one-time login link, returning its user.
func (s *Store) UseWebLogin(ctx context.Context, hash string) (int64, error) {
	uid, err := s.WebSessionUser(ctx, hash, "login")
	if err != nil {
		return 0, err
	}
	res, err := s.db.ExecContext(ctx, "DELETE FROM web_sessions WHERE hash = ? AND kind = 'login'", hash)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return 0, ErrNotFound // used by a concurrent request
	}
	return uid, nil
}

func (s *Store) DeleteWebSession(ctx context.Context, hash string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM web_sessions WHERE hash = ?", hash)
	return err
}

// DaySpend is one UTC day's model spend.
type DaySpend struct {
	Day     time.Time
	CostUSD float64
}

// SpendByDay returns spend per UTC day since t, oldest first.
func (s *Store) SpendByDay(ctx context.Context, since time.Time) ([]DaySpend, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT at / 86400, SUM(cost_usd) FROM usage WHERE at >= ? GROUP BY 1 ORDER BY 1", since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DaySpend
	for rows.Next() {
		var d int64
		var c float64
		if err := rows.Scan(&d, &c); err != nil {
			return nil, err
		}
		out = append(out, DaySpend{Day: time.Unix(d*86400, 0).UTC(), CostUSD: c})
	}
	return out, rows.Err()
}
