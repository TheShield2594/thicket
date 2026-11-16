package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type APIToken struct {
	ID         int64
	UserID     int64
	Name       string
	CreatedAt  time.Time
	LastUsedAt time.Time
}

func (s *Store) CreateAPIToken(ctx context.Context, userID int64, name, hash string) (int64, error) {
	res, err := s.db.ExecContext(ctx, "INSERT INTO api_tokens (user_id, name, hash, created_at) VALUES (?, ?, ?, ?)", userID, name, hash, s.unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// APITokenByHash finds a token and records its use.
func (s *Store) APITokenByHash(ctx context.Context, hash string) (*APIToken, error) {
	var t APIToken
	var created int64
	var used sql.NullInt64
	err := s.db.QueryRowContext(ctx, "SELECT id, user_id, name, created_at, last_used_at FROM api_tokens WHERE hash = ?", hash).
		Scan(&t.ID, &t.UserID, &t.Name, &created, &used)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	t.CreatedAt, t.LastUsedAt = time.Unix(created, 0), timeOf(used)
	_, _ = s.db.ExecContext(ctx, "UPDATE api_tokens SET last_used_at = ? WHERE id = ?", s.unix(), t.ID)
	return &t, nil
}

func (s *Store) ListAPITokens(ctx context.Context) ([]APIToken, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id, user_id, name, created_at, last_used_at FROM api_tokens ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIToken
	for rows.Next() {
		var t APIToken
		var created int64
		var used sql.NullInt64
		if err := rows.Scan(&t.ID, &t.UserID, &t.Name, &created, &used); err != nil {
			return nil, err
		}
		t.CreatedAt, t.LastUsedAt = time.Unix(created, 0), timeOf(used)
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) RevokeAPIToken(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM api_tokens WHERE id = ?", id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
