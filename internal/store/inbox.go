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
	// Action is a draft's exact tool call, as JSON (see DraftAction).
	Action    string
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
			// Still open? It may have been resolved since it was found;
			// then this is a new item.
			res, err := s.db.ExecContext(ctx, "UPDATE inbox SET count = count + 1, title = ?, detail = ?, updated_at = ? WHERE id = ? AND status = 'open'",
				s.clean(it.Title), s.clean(it.Detail), now, id)
			if err != nil {
				return 0, err
			}
			if n, _ := res.RowsAffected(); n == 1 {
				return id, nil
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO inbox (user_id, character, kind, ref, title, detail, action, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, it.UserID, it.Character, it.Kind, it.Ref, s.clean(it.Title), s.clean(it.Detail), it.Action, now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// OpenInbox lists open items, newest first. userID 0 means everyone's.
func (s *Store) OpenInbox(ctx context.Context, userID int64) ([]InboxItem, error) {
	q := "SELECT " + inboxCols + " FROM inbox WHERE status = 'open'"
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
		it, err := scanInbox(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *it)
	}
	return out, rows.Err()
}

const inboxCols = "id, user_id, character, kind, ref, title, detail, count, status, action, created_at, updated_at"

func scanInbox(r interface{ Scan(...any) error }) (*InboxItem, error) {
	var it InboxItem
	var c, u int64
	if err := r.Scan(&it.ID, &it.UserID, &it.Character, &it.Kind, &it.Ref, &it.Title, &it.Detail, &it.Count, &it.Status, &it.Action, &c, &u); err != nil {
		return nil, err
	}
	it.CreatedAt, it.UpdatedAt = time.Unix(c, 0), time.Unix(u, 0)
	return &it, nil
}

// InboxItemByID returns one item, open or not.
func (s *Store) InboxItemByID(ctx context.Context, id int64) (*InboxItem, error) {
	it, err := scanInbox(s.db.QueryRowContext(ctx, "SELECT "+inboxCols+" FROM inbox WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return it, err
}

// OpenInboxFor counts open failed or interrupted items for a sprig newer
// than since (a sprig with some is "stuck").
func (s *Store) OpenInboxFor(ctx context.Context, character string, since time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM inbox WHERE status = 'open' AND character = ?
		AND kind IN ('failed', 'interrupted') AND updated_at >= ?`, character, since.Unix()).Scan(&n)
	return n, err
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

// ApprovalByID returns one approval, whatever its status.
func (s *Store) ApprovalByID(ctx context.Context, id int64) (*Approval, error) {
	var a Approval
	var at int64
	err := s.db.QueryRowContext(ctx, `SELECT id, user_id, character, chat_ref, tool, args, status, result, created_at, estimate_id FROM approvals WHERE id = ?`, id).
		Scan(&a.ID, &a.UserID, &a.Character, &a.ChatRef, &a.Tool, &a.Args, &a.Status, &a.Result, &at, &a.EstimateID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	a.CreatedAt = time.Unix(at, 0)
	return &a, err
}

// ToolRecord is a sprig's track record with one tool, from the trail (the
// activity log) and its approvals.
type ToolRecord struct {
	Actions  int // calls that ran and worked (directly or once approved)
	Failures int // calls that ran and failed
	Drafts   int // actions written up at draft level
	Refused  int // calls refused at read level
	Approved int
	Denied   int
	Expired  int // approval requests nobody answered
}

// ToolRecordSince builds a sprig's track record with a tool.
func (s *Store) ToolRecordSince(ctx context.Context, character, tool string, since time.Time) (ToolRecord, error) {
	var r ToolRecord
	err := s.db.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(summary IN (?, ?)), 0),
		COALESCE(SUM(summary IN (?, ?)), 0),
		COALESCE(SUM(summary = ?), 0),
		COALESCE(SUM(summary = ?), 0)
		FROM activity WHERE character = ? AND at >= ?`,
		"called "+tool, "ran approved tool "+tool,
		"tool "+tool+" failed", "approved tool "+tool+" failed",
		"drafted "+tool, "refused "+tool+": read-only access",
		character, since.Unix()).Scan(&r.Actions, &r.Failures, &r.Drafts, &r.Refused)
	if err != nil {
		return r, err
	}
	err = s.db.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(status = 'approved'), 0), COALESCE(SUM(status = 'denied'), 0), COALESCE(SUM(status = 'expired'), 0)
		FROM approvals WHERE character = ? AND tool = ? AND created_at >= ?`, character, tool, since.Unix()).
		Scan(&r.Approved, &r.Denied, &r.Expired)
	return r, err
}

// PendingApprovals lists approvals still waiting for an answer (younger
// than maxAge). userID 0 means everyone's.
func (s *Store) PendingApprovals(ctx context.Context, userID int64, maxAge time.Duration) ([]Approval, error) {
	q := `SELECT id, user_id, character, chat_ref, tool, args, status, result, created_at, estimate_id FROM approvals
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
		if err := rows.Scan(&a.ID, &a.UserID, &a.Character, &a.ChatRef, &a.Tool, &a.Args, &a.Status, &a.Result, &at, &a.EstimateID); err != nil {
			return nil, err
		}
		a.CreatedAt = time.Unix(at, 0)
		out = append(out, a)
	}
	return out, rows.Err()
}
