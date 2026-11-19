package store

import (
	"context"
	"database/sql"
	"errors"
)

// ExecAllowed reports whether a sprig has opted in to exec-capable tools.
// A failed read is an error, which callers treat as no.
func (s *Store) ExecAllowed(ctx context.Context, character string) (bool, error) {
	var v int
	err := s.db.QueryRowContext(ctx, "SELECT exec FROM sprig_access WHERE character = ?", character).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return v == 1, err
}

// SetExecAllowed turns a sprig's exec opt-in on or off.
func (s *Store) SetExecAllowed(ctx context.Context, character string, on bool, by int64) error {
	v := 0
	if on {
		v = 1
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO sprig_access (character, exec, updated_by, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(character) DO UPDATE SET exec = excluded.exec, updated_by = excluded.updated_by, updated_at = excluded.updated_at`,
		character, v, nullID(by), s.unix())
	return err
}

// PathGrants lists the extra paths a sprig may use, sorted.
func (s *Store) PathGrants(ctx context.Context, character string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT path FROM path_grants WHERE character = ? ORDER BY path", character)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GrantPath adds a path for a sprig. It reports whether it was new.
func (s *Store) GrantPath(ctx context.Context, character, path string, by int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO path_grants (character, path, granted_by, created_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(character, path) DO NOTHING`, character, path, nullID(by), s.unix())
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// RevokePath removes a path from a sprig. It reports whether it was there.
func (s *Store) RevokePath(ctx context.Context, character, path string) (bool, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM path_grants WHERE character = ? AND path = ?", character, path)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}
