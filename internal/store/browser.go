package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"sort"
)

// A sprig's browser allowlist (docs/browser.md): the sites its browser
// may reach. It is kept as a JSON list in the settings table under
// browserAllowKey, so the feature needs no schema change; only its
// sprig's owner changes it, and every change is written in the same
// transaction as its trail entry. It isn't a known setting, so the
// settings screen neither shows nor changes it.

const browserAllowPrefix = "browser.allow."

func browserAllowKey(sprig string) string { return browserAllowPrefix + sprig }

func readAllow(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, key string) ([]string, error) {
	var v string
	err := q.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []string{}
	if err := json.Unmarshal([]byte(v), &out); err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// BrowserAllow returns a sprig's browser allowlist, sorted.
func (s *Store) BrowserAllow(ctx context.Context, sprig string) ([]string, error) {
	return readAllow(ctx, s.db, browserAllowKey(sprig))
}

// ChangeBrowserAllow adds and removes allowlist entries (already
// normalised) in one transaction and reports which really changed.
// trail, if not nil, makes the trail entry for what changed, written in
// the same transaction.
func (s *Store) ChangeBrowserAllow(ctx context.Context, sprig string, add, remove []string, trail func(added, removed []string) *TrailEntry) (added, removed []string, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	key := browserAllowKey(sprig)
	cur, err := readAllow(ctx, tx, key)
	if err != nil {
		return nil, nil, err
	}
	for _, e := range add {
		if !slices.Contains(cur, e) {
			cur = append(cur, e)
			added = append(added, e)
		}
	}
	for _, e := range remove {
		if i := slices.Index(cur, e); i >= 0 {
			cur = slices.Delete(cur, i, i+1)
			removed = append(removed, e)
		}
	}
	if len(added)+len(removed) == 0 {
		return nil, nil, nil
	}
	sort.Strings(cur)
	now := s.unix()
	if len(cur) == 0 {
		_, err = tx.ExecContext(ctx, "DELETE FROM settings WHERE key = ?", key)
	} else {
		b, _ := json.Marshal(cur)
		_, err = tx.ExecContext(ctx, `INSERT INTO settings (key, value, updated_at) VALUES (?, ?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, key, string(b), now)
	}
	if err != nil {
		return nil, nil, err
	}
	if trail != nil {
		if t := trail(added, removed); t != nil {
			if _, err := tx.ExecContext(ctx, "INSERT INTO trail (at, user_id, character, kind, summary, detail, undo) VALUES (?, ?, ?, ?, ?, ?, ?)",
				now, nullInt(t.UserID), t.Character, t.Kind, s.clean(t.Summary), s.clean(t.Detail), string(t.Undo)); err != nil {
				return nil, nil, err
			}
		}
	}
	return added, removed, tx.Commit()
}
