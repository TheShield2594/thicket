package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type Memory struct {
	ID         int64     `json:"id"`
	UserID     int64     `json:"user_id"`
	Character  string    `json:"character"` // "" = shared
	Kind       string    `json:"kind"`
	Content    string    `json:"content"`
	Source     string    `json:"source"` // auto | user
	Locked     bool      `json:"locked"` // user said/edited it; automatic review never changes it
	DueAt      time.Time `json:"due_at,omitempty"`
	ExpiresAt  time.Time `json:"expires_at,omitempty"`
	LastUsedAt time.Time `json:"last_used_at,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

const memCols = "id, user_id, character, kind, content, source, locked, due_at, expires_at, last_used_at, created_at, updated_at"

func scanMemory(r interface{ Scan(...any) error }) (*Memory, error) {
	var m Memory
	var locked int
	var due, exp, used sql.NullInt64
	var created, updated int64
	if err := r.Scan(&m.ID, &m.UserID, &m.Character, &m.Kind, &m.Content, &m.Source, &locked, &due, &exp, &used, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	m.Locked = locked == 1
	m.DueAt, m.ExpiresAt, m.LastUsedAt = timeOf(due), timeOf(exp), timeOf(used)
	m.CreatedAt, m.UpdatedAt = time.Unix(created, 0), time.Unix(updated, 0)
	return &m, nil
}

func (s *Store) queryMemories(ctx context.Context, q string, args ...any) ([]Memory, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Memory
	for rows.Next() {
		m, err := scanMemory(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *m)
	}
	return out, rows.Err()
}

// VisibleMemories returns live memories a character may see for a user:
// the shared layer plus that character's own space.
func (s *Store) VisibleMemories(ctx context.Context, userID int64, character string) ([]Memory, error) {
	return s.queryMemories(ctx, "SELECT "+memCols+` FROM memories WHERE user_id = ? AND (character = '' OR character = ?)
		AND (expires_at IS NULL OR expires_at > ?) ORDER BY id`, userID, character, s.unix())
}

// AllMemories returns every live memory for a user across characters.
func (s *Store) AllMemories(ctx context.Context, userID int64) ([]Memory, error) {
	return s.queryMemories(ctx, "SELECT "+memCols+` FROM memories WHERE user_id = ? AND (expires_at IS NULL OR expires_at > ?) ORDER BY id`, userID, s.unix())
}

func (s *Store) MemoryByID(ctx context.Context, id int64) (*Memory, error) {
	return scanMemory(s.db.QueryRowContext(ctx, "SELECT "+memCols+" FROM memories WHERE id = ?", id))
}

// DueFollowups returns follow-up memories whose time has come.
func (s *Store) DueFollowups(ctx context.Context) ([]Memory, error) {
	now := s.unix()
	return s.queryMemories(ctx, "SELECT "+memCols+` FROM memories WHERE kind = 'followup' AND due_at IS NOT NULL AND due_at <= ?
		AND (expires_at IS NULL OR expires_at > ?) ORDER BY due_at LIMIT 20`, now, now)
}

// MemoryChange is one write to the memories table. All automatic changes go
// through ApplyMemoryChanges so they are recorded for rollback.
type MemoryChange struct {
	Action string // add | update | expire | delete | touch
	Memory Memory // for add: the new memory; for update: ID + new Content/Kind/DueAt
}

func memJSON(m *Memory) string {
	if m == nil {
		return ""
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// ApplyMemoryChanges writes changes in one transaction and logs each one
// against runID (0 for user-initiated changes). It returns the IDs touched.
func (s *Store) ApplyMemoryChanges(ctx context.Context, runID int64, changes []MemoryChange) ([]int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := s.unix()
	var ids []int64
	for _, ch := range changes {
		m := ch.Memory
		var before *Memory
		if ch.Action != "add" {
			before, err = scanMemory(tx.QueryRowContext(ctx, "SELECT "+memCols+" FROM memories WHERE id = ?", m.ID))
			if err != nil {
				return nil, fmt.Errorf("memory %d: %w", m.ID, err)
			}
		}
		switch ch.Action {
		case "add":
			locked := 0
			if m.Locked {
				locked = 1
			}
			res, err := tx.ExecContext(ctx, `INSERT INTO memories (user_id, character, kind, content, source, locked, due_at, expires_at, created_at, updated_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, m.UserID, m.Character, m.Kind, m.Content, m.Source, locked,
				unixOrNil(m.DueAt), unixOrNil(m.ExpiresAt), now, now)
			if err != nil {
				return nil, err
			}
			m.ID, _ = res.LastInsertId()
		case "update":
			kind := m.Kind
			if kind == "" {
				kind = before.Kind
			}
			due := unixOrNil(m.DueAt)
			if m.DueAt.IsZero() {
				due = unixOrNil(before.DueAt)
			}
			if _, err := tx.ExecContext(ctx, "UPDATE memories SET content = ?, kind = ?, due_at = ?, updated_at = ? WHERE id = ?",
				m.Content, kind, due, now, m.ID); err != nil {
				return nil, err
			}
		case "expire":
			if _, err := tx.ExecContext(ctx, "UPDATE memories SET expires_at = ?, updated_at = ? WHERE id = ?", now, now, m.ID); err != nil {
				return nil, err
			}
		case "delete":
			if _, err := tx.ExecContext(ctx, "DELETE FROM memories WHERE id = ?", m.ID); err != nil {
				return nil, err
			}
			// Forgetting must actually forget: scrub the history too.
			if _, err := tx.ExecContext(ctx, "DELETE FROM memory_events WHERE memory_id = ?", m.ID); err != nil {
				return nil, err
			}
			ids = append(ids, m.ID)
			continue
		case "touch":
			if _, err := tx.ExecContext(ctx, "UPDATE memories SET updated_at = ? WHERE id = ?", now, m.ID); err != nil {
				return nil, err
			}
			ids = append(ids, m.ID)
			continue
		default:
			return nil, fmt.Errorf("unknown memory action %q", ch.Action)
		}
		after, err := scanMemory(tx.QueryRowContext(ctx, "SELECT "+memCols+" FROM memories WHERE id = ?", m.ID))
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO memory_events (memory_id, run_id, action, before, after, created_at) VALUES (?, ?, ?, ?, ?, ?)",
			m.ID, nullInt(runID), ch.Action, memJSON(before), memJSON(after), now); err != nil {
			return nil, err
		}
		ids = append(ids, m.ID)
	}
	return ids, tx.Commit()
}

// LockMemory marks a memory as user-owned: automatic review won't touch it.
func (s *Store) LockMemory(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, "UPDATE memories SET locked = 1, source = 'user' WHERE id = ?", id)
	return err
}

func (s *Store) MarkMemoriesUsed(ctx context.Context, ids []int64) error {
	now := s.unix()
	for _, id := range ids {
		if _, err := s.db.ExecContext(ctx, "UPDATE memories SET last_used_at = ? WHERE id = ?", now, id); err != nil {
			return err
		}
	}
	return nil
}

// SweepMemories hard-deletes memories that expired more than grace ago.
// The grace window is what makes rollback possible.
func (s *Store) SweepMemories(ctx context.Context, grace time.Duration) (int64, error) {
	cutoff := s.unix() - int64(grace.Seconds())
	res, err := s.db.ExecContext(ctx, "DELETE FROM memories WHERE expires_at IS NOT NULL AND expires_at < ?", cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// --- extraction runs ---

type ExtractionRun struct {
	ID             int64
	ConversationID int64
	UserID         int64
	Model          string
	Status         string
	Error          string
	Applied        int
	Skipped        int
	RolledBackAt   time.Time
	CreatedAt      time.Time
}

func (s *Store) CreateExtractionRun(ctx context.Context, convID, userID int64, model string) (int64, error) {
	res, err := s.db.ExecContext(ctx, "INSERT INTO extraction_runs (conversation_id, user_id, model, status, created_at) VALUES (?, ?, ?, 'running', ?)",
		nullInt(convID), userID, model, s.unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) FinishExtractionRun(ctx context.Context, id int64, status, errText string, applied, skipped int) error {
	_, err := s.db.ExecContext(ctx, "UPDATE extraction_runs SET status = ?, error = ?, applied = ?, skipped = ? WHERE id = ?", status, errText, applied, skipped, id)
	return err
}

func (s *Store) ListExtractionRuns(ctx context.Context, limit int) ([]ExtractionRun, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, COALESCE(conversation_id, 0), user_id, model, status, error, applied, skipped, rolled_back_at, created_at
		FROM extraction_runs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ExtractionRun
	for rows.Next() {
		var r ExtractionRun
		var rb sql.NullInt64
		var created int64
		if err := rows.Scan(&r.ID, &r.ConversationID, &r.UserID, &r.Model, &r.Status, &r.Error, &r.Applied, &r.Skipped, &rb, &created); err != nil {
			return nil, err
		}
		r.RolledBackAt, r.CreatedAt = timeOf(rb), time.Unix(created, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}

// RollbackRun reverts every memory change an extraction run made, newest first.
func (s *Store) RollbackRun(ctx context.Context, runID int64) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var rolled sql.NullInt64
	if err := tx.QueryRowContext(ctx, "SELECT rolled_back_at FROM extraction_runs WHERE id = ?", runID).Scan(&rolled); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, err
	}
	if rolled.Valid {
		return 0, fmt.Errorf("run %d was already rolled back", runID)
	}
	rows, err := tx.QueryContext(ctx, "SELECT memory_id, action, COALESCE(before, '') FROM memory_events WHERE run_id = ? ORDER BY id DESC", runID)
	if err != nil {
		return 0, err
	}
	type ev struct {
		id     int64
		action string
		before string
	}
	var evs []ev
	for rows.Next() {
		var e ev
		if err := rows.Scan(&e.id, &e.action, &e.before); err != nil {
			rows.Close()
			return 0, err
		}
		evs = append(evs, e)
	}
	rows.Close()
	now := s.unix()
	for _, e := range evs {
		switch e.action {
		case "add":
			if _, err := tx.ExecContext(ctx, "DELETE FROM memories WHERE id = ?", e.id); err != nil {
				return 0, err
			}
		case "update", "expire":
			var b Memory
			if err := json.Unmarshal([]byte(e.before), &b); err != nil {
				return 0, fmt.Errorf("memory %d: unreadable history: %w", e.id, err)
			}
			if _, err := tx.ExecContext(ctx, "UPDATE memories SET content = ?, kind = ?, due_at = ?, expires_at = ?, updated_at = ? WHERE id = ?",
				b.Content, b.Kind, unixOrNil(b.DueAt), unixOrNil(b.ExpiresAt), now, e.id); err != nil {
				return 0, err
			}
		}
	}
	if _, err := tx.ExecContext(ctx, "UPDATE extraction_runs SET rolled_back_at = ? WHERE id = ?", now, runID); err != nil {
		return 0, err
	}
	return len(evs), tx.Commit()
}
