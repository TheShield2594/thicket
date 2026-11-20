package store

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"time"
)

// MailAccount is a sprig's own email address, served by the thicket-mail
// plugin. The password is in the secret store under SecretName.
type MailAccount struct {
	Sprig         string
	Address       string
	Backend       string // jmap | imap
	Settings      string // JSON: server addresses and username (no secrets)
	SecretName    string
	RetentionDays int
	SendLimit     int
	CreatedBy     int64
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

const mailCols = "sprig, address, backend, settings, secret_name, retention_days, send_limit, created_by, created_at, updated_at"

func scanMail(r interface{ Scan(...any) error }) (*MailAccount, error) {
	var a MailAccount
	var c, u int64
	if err := r.Scan(&a.Sprig, &a.Address, &a.Backend, &a.Settings, &a.SecretName, &a.RetentionDays, &a.SendLimit, &a.CreatedBy, &c, &u); err != nil {
		return nil, err
	}
	a.CreatedAt, a.UpdatedAt = time.Unix(c, 0), time.Unix(u, 0)
	return &a, nil
}

// PutMailAccount stores a sprig's address, replacing any it had (who set
// it up first is kept).
func (s *Store) PutMailAccount(ctx context.Context, a MailAccount) error {
	now := s.unix()
	_, err := s.db.ExecContext(ctx, `INSERT INTO mail_accounts (`+mailCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(sprig) DO UPDATE SET address = excluded.address, backend = excluded.backend, settings = excluded.settings,
		secret_name = excluded.secret_name, retention_days = excluded.retention_days, send_limit = excluded.send_limit, updated_at = excluded.updated_at`,
		a.Sprig, a.Address, a.Backend, a.Settings, a.SecretName, a.RetentionDays, a.SendLimit, a.CreatedBy, now, now)
	return err
}

// MailAccount returns a sprig's address, or ErrNotFound.
func (s *Store) MailAccount(ctx context.Context, sprig string) (*MailAccount, error) {
	a, err := scanMail(s.db.QueryRowContext(ctx, "SELECT "+mailCols+" FROM mail_accounts WHERE sprig = ?", sprig))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return a, err
}

// MailAccounts lists every sprig address.
func (s *Store) MailAccounts(ctx context.Context) ([]MailAccount, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+mailCols+" FROM mail_accounts ORDER BY sprig")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MailAccount
	for rows.Next() {
		a, err := scanMail(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

// DeleteMailAccount removes a sprig's address (its allowlist stays, so
// setting the address up again brings it back).
func (s *Store) DeleteMailAccount(ctx context.Context, sprig string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM mail_accounts WHERE sprig = ?", sprig)
	return err
}

// MailAllow returns a sprig's mail allowlist, sorted.
func (s *Store) MailAllow(ctx context.Context, sprig string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT entry FROM mail_allow WHERE sprig = ?", sprig)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	sort.Strings(out)
	return out, rows.Err()
}

// ChangeMailAllow adds and removes allowlist entries (already normalised)
// in one transaction, and reports which really changed. trail, if not
// nil, makes the trail entry for what changed; it is written in the same
// transaction, so a change is never made without its record.
func (s *Store) ChangeMailAllow(ctx context.Context, sprig string, add, remove []string, by int64, trail func(added, removed []string) *TrailEntry) (added, removed []string, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	now := s.unix()
	for _, e := range add {
		res, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO mail_allow (sprig, entry, added_by, added_at) VALUES (?, ?, ?, ?)", sprig, e, by, now)
		if err != nil {
			return nil, nil, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			added = append(added, e)
		}
	}
	for _, e := range remove {
		res, err := tx.ExecContext(ctx, "DELETE FROM mail_allow WHERE sprig = ? AND entry = ?", sprig, e)
		if err != nil {
			return nil, nil, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			removed = append(removed, e)
		}
	}
	if trail != nil && len(added)+len(removed) > 0 {
		if t := trail(added, removed); t != nil {
			if _, err := tx.ExecContext(ctx, "INSERT INTO trail (at, user_id, character, kind, summary, detail, undo) VALUES (?, ?, ?, ?, ?, ?, ?)",
				now, nullInt(t.UserID), t.Character, t.Kind, s.clean(t.Summary), s.clean(t.Detail), string(t.Undo)); err != nil {
				return nil, nil, err
			}
		}
	}
	return added, removed, tx.Commit()
}
