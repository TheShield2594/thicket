package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Inbox kinds.
const (
	InboxFailed      = "failed"      // a job or task failed
	InboxInterrupted = "interrupted" // Thicket stopped while it ran
	InboxUndelivered = "undelivered" // it worked, but the message didn't reach the user
	InboxDraft       = "draft"       // a sprig at draft level wrote up an action
)

// InboxItem is something that needs a person. Nothing that fails is
// dropped: it lands here until someone resolves it.
type InboxItem struct {
	ID        int64
	UserID    int64
	Character string
	Kind      string
	Ref       string // job:ID, task:ID or tool:NAME
	Title     string
	Detail    string
	Count     int
	Status    string // open | resolved
	CreatedAt time.Time
	UpdatedAt time.Time
}

// AddInbox records an item. An open item with the same user, kind and
// non-empty ref is bumped (count, detail, time) instead of duplicated, so
// a job failing every hour is one item, not a pile.
func (s *Store) AddInbox(ctx context.Context, it InboxItem) (int64, error) {
	now := s.unix()
	if it.Ref != "" {
		var id int64
		err := s.db.QueryRowContext(ctx, "SELECT id FROM inbox WHERE user_id = ? AND kind = ? AND ref = ? AND status = 'open'",
			it.UserID, it.Kind, it.Ref).Scan(&id)
		if err == nil {
			_, err = s.db.ExecContext(ctx, "UPDATE inbox SET count = count + 1, title = ?, detail = ?, updated_at = ? WHERE id = ?",
				it.Title, it.Detail, now, id)
			return id, err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO inbox (user_id, character, kind, ref, title, detail, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, it.UserID, it.Character, it.Kind, it.Ref, it.Title, it.Detail, now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// OpenInbox lists open items, newest first. userID 0 means everyone's.
func (s *Store) OpenInbox(ctx context.Context, userID int64) ([]InboxItem, error) {
	q := "SELECT id, user_id, character, kind, ref, title, detail, count, status, created_at, updated_at FROM inbox WHERE status = 'open'"
	var args []any
	if userID != 0 {
		q += " AND user_id = ?"
		args = append(args, userID)
	}
	rows, err := s.db.QueryContext(ctx, q+" ORDER BY updated_at DESC, id DESC LIMIT 200", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InboxItem
	for rows.Next() {
		var it InboxItem
		var c, u int64
		if err := rows.Scan(&it.ID, &it.UserID, &it.Character, &it.Kind, &it.Ref, &it.Title, &it.Detail, &it.Count, &it.Status, &c, &u); err != nil {
			return nil, err
		}
		it.CreatedAt, it.UpdatedAt = time.Unix(c, 0), time.Unix(u, 0)
		out = append(out, it)
	}
	return out, rows.Err()
}

// ResolveInbox closes an item. userID 0 skips the ownership check (CLI,
// admins). It returns ErrNotFound if there's no such open item.
func (s *Store) ResolveInbox(ctx context.Context, id, userID int64) error {
	q := "UPDATE inbox SET status = 'resolved', resolved_at = ? WHERE id = ? AND status = 'open'"
	args := []any{s.unix(), id}
	if userID != 0 {
		q += " AND user_id = ?"
		args = append(args, userID)
	}
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// PendingApprovals lists approvals still waiting for an answer (younger
// than maxAge). userID 0 means everyone's.
func (s *Store) PendingApprovals(ctx context.Context, userID int64, maxAge time.Duration) ([]Approval, error) {
	q := `SELECT id, user_id, character, chat_ref, tool, args, status, result, created_at FROM approvals
		WHERE status = 'pending' AND created_at >= ?`
	args := []any{s.unix() - int64(maxAge.Seconds())}
	if userID != 0 {
		q += " AND user_id = ?"
		args = append(args, userID)
	}
	rows, err := s.db.QueryContext(ctx, q+" ORDER BY id DESC", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Approval
	for rows.Next() {
		var a Approval
		var at int64
		if err := rows.Scan(&a.ID, &a.UserID, &a.Character, &a.ChatRef, &a.Tool, &a.Args, &a.Status, &a.Result, &at); err != nil {
			return nil, err
		}
		a.CreatedAt = time.Unix(at, 0)
		out = append(out, a)
	}
	return out, rows.Err()
}
