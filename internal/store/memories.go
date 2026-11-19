package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
	// Household marks family memory: what family sprigs know about the
	// household, apart from anyone's private memories. UserID is who it
	// came from.
	Household bool `json:"household,omitempty"`
	// Origin is where it came from: "review" (the automatic memory
	// review), "sprig:<id>" (a sprig saved it in chat), "person:<user id>"
	// (added on the memory screen), "import", or "" (older memories).
	Origin string `json:"origin,omitempty"`
}

// Origins of memories.
const (
	OriginReview       = "review"
	OriginImport       = "import"
	OriginSprigPrefix  = "sprig:"
	OriginPersonPrefix = "person:"
)

const memCols = "id, user_id, character, kind, content, source, locked, due_at, expires_at, last_used_at, created_at, updated_at, household, origin"

func scanMemory(r interface{ Scan(...any) error }) (*Memory, error) {
	var m Memory
	var locked int
	var due, exp, used sql.NullInt64
	var created, updated int64
	var household int
	if err := r.Scan(&m.ID, &m.UserID, &m.Character, &m.Kind, &m.Content, &m.Source, &locked, &due, &exp, &used, &created, &updated, &household, &m.Origin); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	m.Locked, m.Household = locked == 1, household == 1
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

// VisibleMemories returns a user's live private memories a character may
// see: the shared layer plus that character's own space. Family memory
// is separate (FamilyMemories).
func (s *Store) VisibleMemories(ctx context.Context, userID int64, character string) ([]Memory, error) {
	return s.queryMemories(ctx, "SELECT "+memCols+` FROM memories WHERE user_id = ? AND household = 0 AND (character = '' OR character = ?)
		AND (expires_at IS NULL OR expires_at > ?) ORDER BY id`, userID, character, s.unix())
}

// FamilyMemories returns live family memory a family sprig may see: the
// household layer plus its own household notes.
func (s *Store) FamilyMemories(ctx context.Context, character string) ([]Memory, error) {
	return s.queryMemories(ctx, "SELECT "+memCols+` FROM memories WHERE household = 1 AND (character = '' OR character = ?)
		AND (expires_at IS NULL OR expires_at > ?) ORDER BY id`, character, s.unix())
}

// AllMemories returns every live private memory for a user across
// characters (not family memory).
func (s *Store) AllMemories(ctx context.Context, userID int64) ([]Memory, error) {
	return s.queryMemories(ctx, "SELECT "+memCols+` FROM memories WHERE user_id = ? AND household = 0 AND (expires_at IS NULL OR expires_at > ?) ORDER BY id`, userID, s.unix())
}

func (s *Store) MemoryByID(ctx context.Context, id int64) (*Memory, error) {
	return scanMemory(s.db.QueryRowContext(ctx, "SELECT "+memCols+" FROM memories WHERE id = ?", id))
}

// DueFollowups returns follow-up memories whose time has come.
// FollowupsDueBetween lists one person's follow-ups due in [from, to) that
// a sprig may mention (its own and shared ones, never another sprig's)
// and that haven't expired by now, soonest first, for briefs and due
// reminders. With skipClaimed set (a notice key prefix such as "due:"),
// ones whose notice key prefix+id was already claimed are left out before
// the limit.
func (s *Store) FollowupsDueBetween(ctx context.Context, userID int64, character string, from, to, now time.Time, skipClaimed string) ([]Memory, error) {
	q := "SELECT " + memCols + ` FROM memories WHERE user_id = ? AND kind = 'followup' AND household = 0 AND (character = '' OR character = ?)
		AND due_at IS NOT NULL AND due_at >= ? AND due_at < ? AND (expires_at IS NULL OR expires_at > ?)`
	args := []any{userID, character, from.Unix(), to.Unix(), now.Unix()}
	if skipClaimed != "" {
		q += " AND NOT EXISTS (SELECT 1 FROM notices n WHERE n.key = ? || memories.id)"
		args = append(args, skipClaimed)
	}
	return s.queryMemories(ctx, q+" ORDER BY due_at LIMIT 20", args...)
}

func (s *Store) DueFollowups(ctx context.Context) ([]Memory, error) {
	now := s.unix()
	return s.queryMemories(ctx, "SELECT "+memCols+` FROM memories WHERE kind = 'followup' AND household = 0 AND due_at IS NOT NULL AND due_at <= ?
		AND (expires_at IS NULL OR expires_at > ?) ORDER BY due_at LIMIT 20`, now, now)
}

// KidSprigMemories returns the live private memories a kid sprig sees for
// its kids: each kid's shared layer and the sprig's own space. It is what
// a kid sprig's parents see on the memory screen.
func (s *Store) KidSprigMemories(ctx context.Context, kids []int64, character string) ([]Memory, error) {
	if len(kids) == 0 {
		return nil, nil
	}
	q := "SELECT " + memCols + " FROM memories WHERE household = 0 AND (character = '' OR character = ?) AND (expires_at IS NULL OR expires_at > ?) AND user_id IN (?" +
		strings.Repeat(", ?", len(kids)-1) + ") ORDER BY id"
	args := []any{character, s.unix()}
	for _, k := range kids {
		args = append(args, k)
	}
	return s.queryMemories(ctx, q, args...)
}

// MemoryChange is one write to the memories table. All automatic changes go
// through ApplyMemoryChanges so they are recorded for rollback.
type MemoryChange struct {
	Action string // add | update | expire | delete | touch | lock
	// Memory is, for add: the new memory; for update: ID + new
	// Content/Kind/DueAt, where an empty field keeps the current value
	// (read inside the transaction, so a concurrent edit survives); for
	// lock: ID + Locked.
	Memory Memory
	// SetExpiry makes an update write Memory.ExpiresAt (zero clears it),
	// e.g. when a memory changes to or from the short-lived "context" kind.
	SetExpiry bool
	// SetLock makes an update write Memory.Locked too, in the same event,
	// so undoing a person's edit undoes the lock that came with it.
	SetLock bool
	// By is the person who made the change on the memory screen (0 for
	// sprigs and the review). Their changes can be undone one by one.
	By int64
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
			res, err := tx.ExecContext(ctx, `INSERT INTO memories (user_id, character, kind, content, source, locked, due_at, expires_at, created_at, updated_at, household, origin)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, m.UserID, m.Character, m.Kind, s.clean(m.Content), m.Source, locked,
				unixOrNil(m.DueAt), unixOrNil(m.ExpiresAt), now, now, boolInt(m.Household), m.Origin)
			if err != nil {
				return nil, err
			}
			m.ID, _ = res.LastInsertId()
		case "update":
			// Re-check the lock inside the transaction: the user may have
			// locked it while an automatic review was running.
			if runID != 0 && before.Locked {
				continue
			}
			content, kind := m.Content, m.Kind
			if content == "" {
				content = before.Content
			}
			if kind == "" {
				kind = before.Kind
			}
			due := unixOrNil(m.DueAt)
			if m.DueAt.IsZero() {
				due = unixOrNil(before.DueAt)
			}
			exp := unixOrNil(before.ExpiresAt)
			if ch.SetExpiry {
				exp = unixOrNil(m.ExpiresAt)
			}
			locked, source := boolInt(before.Locked), before.Source
			if ch.SetLock {
				locked, source = boolInt(m.Locked), lockSource(m.Locked)
			}
			if _, err := tx.ExecContext(ctx, "UPDATE memories SET content = ?, kind = ?, due_at = ?, expires_at = ?, locked = ?, source = ?, updated_at = ? WHERE id = ?",
				s.clean(content), kind, due, exp, locked, source, now, m.ID); err != nil {
				return nil, err
			}
		case "lock":
			if _, err := tx.ExecContext(ctx, "UPDATE memories SET locked = ?, source = ?, updated_at = ? WHERE id = ?", boolInt(m.Locked), lockSource(m.Locked), now, m.ID); err != nil {
				return nil, err
			}
		case "expire":
			if runID != 0 && before.Locked {
				continue
			}
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
		if _, err := tx.ExecContext(ctx, "INSERT INTO memory_events (memory_id, run_id, action, before, after, created_at, by_user) VALUES (?, ?, ?, ?, ?, ?, ?)",
			m.ID, nullInt(runID), ch.Action, memJSON(before), memJSON(after), now, nullInt(ch.By)); err != nil {
			return nil, err
		}
		ids = append(ids, m.ID)
	}
	return ids, tx.Commit()
}

// lockSource is the source a lock change leaves: a locked memory is the
// person's ("user"); unlocking hands it back to the review ("auto"), as
// the doctor expects of every unlocked memory. Origin still says who
// first wrote it.
func lockSource(locked bool) string {
	if locked {
		return "user"
	}
	return "auto"
}

// UndoableMemories reports which of the given memories have a change a
// person made on the memory screen that can still be undone.
func (s *Store) UndoableMemories(ctx context.Context, ids []int64) (map[int64]bool, error) {
	out := map[int64]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT memory_id FROM memory_events WHERE by_user IS NOT NULL AND undone_at IS NULL
		AND action != 'undo' AND memory_id IN (?`+strings.Repeat(", ?", len(ids)-1)+")", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// ErrNothingToUndo means a memory has no change from the memory screen
// left to undo.
var ErrNothingToUndo = errors.New("nothing to undo: only changes made on the memory screen can be undone")

// ErrUndoConflict means what a change set has been changed again since
// (by the review, a sprig or another edit), so undoing it would throw
// that later change away.
var ErrUndoConflict = errors.New("can't undo: this memory was changed again since; edit it instead")

// UndoMemoryChange reverts the newest change a person made to a memory on
// the memory screen that isn't undone yet, and records the undo. Undoing
// an add removes the memory (nil is returned). Calling it again walks
// further back. Deleting is never undoable: forgetting forgets.
//
// Only the fields that change touched are put back; anything else changed
// since (say, the review rewording a memory after it was unlocked) stays.
// If a field the change set has been changed again, the undo is refused
// (ErrUndoConflict) rather than losing that later change.
func (s *Store) UndoMemoryChange(ctx context.Context, memoryID, by int64) (*Memory, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var evID int64
	var action, before, evAfter string
	err = tx.QueryRowContext(ctx, `SELECT id, action, COALESCE(before, ''), COALESCE(after, '') FROM memory_events WHERE memory_id = ? AND by_user IS NOT NULL
		AND undone_at IS NULL AND action != 'undo' ORDER BY id DESC LIMIT 1`, memoryID).Scan(&evID, &action, &before, &evAfter)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNothingToUndo
	} else if err != nil {
		return nil, err
	}
	cur, err := scanMemory(tx.QueryRowContext(ctx, "SELECT "+memCols+" FROM memories WHERE id = ?", memoryID))
	if err != nil {
		return nil, err
	}
	now := s.unix()
	if action == "add" {
		// Undoing an add takes the memory away, history and all.
		if _, err := tx.ExecContext(ctx, "DELETE FROM memories WHERE id = ?", memoryID); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM memory_events WHERE memory_id = ?", memoryID); err != nil {
			return nil, err
		}
		return nil, tx.Commit()
	}
	var b, a Memory
	if err := json.Unmarshal([]byte(before), &b); err != nil {
		return nil, fmt.Errorf("memory %d: unreadable history: %w", memoryID, err)
	}
	if err := json.Unmarshal([]byte(evAfter), &a); err != nil {
		return nil, fmt.Errorf("memory %d: unreadable history: %w", memoryID, err)
	}
	r, err := undoFields(*cur, b, a)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE memories SET content = ?, kind = ?, due_at = ?, expires_at = ?, locked = ?, source = ?, updated_at = ? WHERE id = ?",
		r.Content, r.Kind, unixOrNil(r.DueAt), unixOrNil(r.ExpiresAt), boolInt(r.Locked), r.Source, now, memoryID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE memory_events SET undone_at = ? WHERE id = ?", now, evID); err != nil {
		return nil, err
	}
	after, err := scanMemory(tx.QueryRowContext(ctx, "SELECT "+memCols+" FROM memories WHERE id = ?", memoryID))
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO memory_events (memory_id, action, before, after, created_at, by_user) VALUES (?, 'undo', ?, ?, ?, ?)",
		memoryID, memJSON(cur), memJSON(after), now, nullInt(by)); err != nil {
		return nil, err
	}
	return after, tx.Commit()
}

// undoFields returns cur with each field that a change moved from before
// to after put back to before. A field the change moved that no longer
// holds what the change left is a conflict.
func undoFields(cur, before, after Memory) (Memory, error) {
	r := cur
	conflict := false
	str := func(dst *string, c, b, a string) {
		if b == a {
			return
		}
		if c != a {
			conflict = true
		}
		*dst = b
	}
	tm := func(dst *time.Time, c, b, a time.Time) {
		if b.Equal(a) {
			return
		}
		if !c.Equal(a) {
			conflict = true
		}
		*dst = b
	}
	str(&r.Content, cur.Content, before.Content, after.Content)
	str(&r.Kind, cur.Kind, before.Kind, after.Kind)
	tm(&r.DueAt, cur.DueAt, before.DueAt, after.DueAt)
	tm(&r.ExpiresAt, cur.ExpiresAt, before.ExpiresAt, after.ExpiresAt)
	// The lock and its source go together.
	if before.Locked != after.Locked || before.Source != after.Source {
		if cur.Locked != after.Locked || cur.Source != after.Source {
			conflict = true
		}
		r.Locked, r.Source = before.Locked, before.Source
	}
	if conflict {
		return cur, ErrUndoConflict
	}
	return r, nil
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	// History holds full copies of the content, so it goes with the memory.
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_events WHERE memory_id IN
		(SELECT id FROM memories WHERE expires_at IS NOT NULL AND expires_at < ?)`, cutoff); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, "DELETE FROM memories WHERE expires_at IS NOT NULL AND expires_at < ?", cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, tx.Commit()
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
	_, err := s.db.ExecContext(ctx, "UPDATE extraction_runs SET status = ?, error = ?, applied = ?, skipped = ? WHERE id = ?", status, s.clean(errText), applied, skipped, id)
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
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err // never mark a partial rollback as complete
	}
	rows.Close()
	now := s.unix()
	for _, e := range evs {
		switch e.action {
		case "add":
			if _, err := tx.ExecContext(ctx, "DELETE FROM memories WHERE id = ?", e.id); err != nil {
				return 0, err
			}
			// The memory is gone: drop its other history (NULL-safe, so
			// user-initiated events go too) and keep this run's events only
			// as a record, without the content snapshots.
			if _, err := tx.ExecContext(ctx, "DELETE FROM memory_events WHERE memory_id = ? AND (run_id IS NULL OR run_id != ?)", e.id, runID); err != nil {
				return 0, err
			}
			if _, err := tx.ExecContext(ctx, "UPDATE memory_events SET before = NULL, after = NULL WHERE memory_id = ? AND run_id = ?", e.id, runID); err != nil {
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
