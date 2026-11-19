package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// What a token is for (api_tokens.kind), enforced by the server.
const (
	TokenCLI = "cli" // scripts and the local API: thicket api-token create
	TokenMCP = "mcp" // an AI agent through thicket mcp: no approvals, no admin
	TokenApp = "app" // signed in through POST /api/v2/auth/login
)

type APIToken struct {
	ID         int64
	UserID     int64
	Name       string
	Kind       string
	CreatedAt  time.Time
	LastUsedAt time.Time
}

// CreateAPIToken makes a cli token.
func (s *Store) CreateAPIToken(ctx context.Context, userID int64, name, hash string) (int64, error) {
	return s.CreateTokenOfKind(ctx, userID, name, TokenCLI, hash)
}

// CreateTokenOfKind makes a token of kind TokenCLI, TokenMCP or TokenApp.
func (s *Store) CreateTokenOfKind(ctx context.Context, userID int64, name, kind, hash string) (int64, error) {
	res, err := s.db.ExecContext(ctx, "INSERT INTO api_tokens (user_id, name, kind, hash, created_at) VALUES (?, ?, ?, ?, ?)", userID, name, kind, hash, s.unix())
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
	err := s.db.QueryRowContext(ctx, "SELECT id, user_id, name, kind, created_at, last_used_at FROM api_tokens WHERE hash = ?", hash).
		Scan(&t.ID, &t.UserID, &t.Name, &t.Kind, &created, &used)
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
	rows, err := s.db.QueryContext(ctx, "SELECT id, user_id, name, kind, created_at, last_used_at FROM api_tokens ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIToken
	for rows.Next() {
		var t APIToken
		var created int64
		var used sql.NullInt64
		if err := rows.Scan(&t.ID, &t.UserID, &t.Name, &t.Kind, &created, &used); err != nil {
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

// AppTokenPrefix starts the name of a token the apps get at sign-in.
const AppTokenPrefix = "app: "
