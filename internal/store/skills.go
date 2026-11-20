package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Skill is one version of a skill (the current one unless asked).
type Skill struct {
	ID          string
	Version     int
	Name        string
	Description string
	Body        string
	Note        string
	Author      string
	UserID      int64
	CreatedAt   time.Time // of this version
	// SprigID is the sprig that owns the skill ("" = the shared library).
	// Only that sprig is offered it. Set when the skill is created; a
	// revision keeps the skill's owner whatever it says.
	SprigID string
	// Optional, for taught skills: what the skill needs to be told, and
	// how to tell it worked.
	Inputs       []SkillInput
	SuccessCheck string
}

// SkillInput is one thing a skill needs to be told before it runs.
type SkillInput struct {
	Name        string `json:"name" yaml:"name"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
	Required    bool   `json:"required,omitempty" yaml:"required,omitempty"`
}

// Shared reports whether the skill is in the shared library.
func (k *Skill) Shared() bool { return k.SprigID == "" }

// OfferedTo reports whether a sprig is offered this skill.
func (k *Skill) OfferedTo(sprig string) bool { return k.SprigID == "" || k.SprigID == sprig }

// MaxSkills keeps the shared library (and the index in every prompt)
// small. MaxSprigSkills is the same for each sprig's own skills.
const (
	MaxSkills      = 100
	MaxSprigSkills = 40
)

var ErrNoChange = errors.New("nothing changed")

// ErrSkillIDTaken means a deleted skill's id can't be reused by this owner.
var ErrSkillIDTaken = errors.New("skill id taken")

// VersionConflict means the skill changed since the caller read it.
type VersionConflict struct{ Current int }

func (e *VersionConflict) Error() string {
	return fmt.Sprintf("the skill changed since you read it (now version %d): read it again and redo your change on top", e.Current)
}

const skillCols = "s.id, v.version, v.name, v.description, v.body, v.note, v.author, v.user_id, v.created_at, s.sprig_id, v.inputs, v.success_check"

func scanSkill(r interface{ Scan(...any) error }) (*Skill, error) {
	var k Skill
	var uid sql.NullInt64
	var sprig sql.NullString
	var inputs string
	var at int64
	if err := r.Scan(&k.ID, &k.Version, &k.Name, &k.Description, &k.Body, &k.Note, &k.Author, &uid, &at, &sprig, &inputs, &k.SuccessCheck); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	k.UserID, k.CreatedAt, k.SprigID = uid.Int64, time.Unix(at, 0), sprig.String
	if inputs != "" {
		if err := json.Unmarshal([]byte(inputs), &k.Inputs); err != nil {
			return nil, fmt.Errorf("skill %s inputs: %w", k.ID, err)
		}
	}
	return &k, nil
}

func encodeInputs(in []SkillInput) (string, error) {
	if len(in) == 0 {
		return "", nil
	}
	b, err := json.Marshal(in)
	return string(b), err
}

func (s *Store) querySkills(ctx context.Context, where string, args ...any) ([]Skill, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+skillCols+" FROM skills s JOIN skill_versions v ON v.skill_id = s.id AND v.version = s.version WHERE s.deleted = 0"+where+" ORDER BY s.id", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Skill
	for rows.Next() {
		k, err := scanSkill(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *k)
	}
	return out, rows.Err()
}

// Skills returns the current version of every skill, shared and
// sprig-owned, by id.
func (s *Store) Skills(ctx context.Context) ([]Skill, error) {
	return s.querySkills(ctx, "")
}

// SkillsFor returns the skills a sprig is offered: the shared library and
// its own, by id.
func (s *Store) SkillsFor(ctx context.Context, sprig string) ([]Skill, error) {
	return s.querySkills(ctx, " AND (s.sprig_id IS NULL OR s.sprig_id = ?)", sprig)
}

// SprigSkills returns only the skills a sprig owns.
func (s *Store) SprigSkills(ctx context.Context, sprig string) ([]Skill, error) {
	return s.querySkills(ctx, " AND s.sprig_id = ?", sprig)
}

// SkillOwners maps every sprig-owned skill's id to its sprig.
func (s *Store) SkillOwners(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id, sprig_id FROM skills WHERE deleted = 0 AND sprig_id IS NOT NULL")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, sprig string
		if err := rows.Scan(&id, &sprig); err != nil {
			return nil, err
		}
		out[id] = sprig
	}
	return out, rows.Err()
}

// SkillByID returns a skill's current version, or version v if v > 0.
func (s *Store) SkillByID(ctx context.Context, id string, v int) (*Skill, error) {
	if v > 0 {
		return scanSkill(s.db.QueryRowContext(ctx, "SELECT "+skillCols+" FROM skills s JOIN skill_versions v ON v.skill_id = s.id WHERE s.id = ? AND s.deleted = 0 AND v.version = ?", id, v))
	}
	return scanSkill(s.db.QueryRowContext(ctx, "SELECT "+skillCols+" FROM skills s JOIN skill_versions v ON v.skill_id = s.id AND v.version = s.version WHERE s.id = ? AND s.deleted = 0", id))
}

// SkillHistory lists a skill's versions, newest first (bodies included).
func (s *Store) SkillHistory(ctx context.Context, id string) ([]Skill, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+skillCols+" FROM skills s JOIN skill_versions v ON v.skill_id = s.id WHERE s.id = ? AND s.deleted = 0 ORDER BY v.version DESC", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Skill
	for rows.Next() {
		k, err := scanSkill(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *k)
	}
	return out, rows.Err()
}

// SaveSkill adds a version. base is the version the change was made on: 0
// to create a new skill, else the current version, or *VersionConflict.
// A save identical to the current version returns ErrNoChange.
func (s *Store) SaveSkill(ctx context.Context, k Skill, base int) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	now := s.unix()
	var cur int
	var deleted bool
	inputs, err := encodeInputs(k.Inputs)
	if err != nil {
		return 0, err
	}
	var owner any
	if k.SprigID != "" {
		owner = k.SprigID
	}
	err = tx.QueryRowContext(ctx, "SELECT version, deleted FROM skills WHERE id = ?", k.ID).Scan(&cur, &deleted)
	switch {
	case errors.Is(err, sql.ErrNoRows), err == nil && deleted:
		if base != 0 {
			// Gone, or deleted since it was read (maybe recreated later).
			return 0, ErrNotFound
		}
		var n int
		if k.SprigID == "" {
			if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM skills WHERE deleted = 0 AND sprig_id IS NULL").Scan(&n); err != nil {
				return 0, err
			}
			if n >= MaxSkills {
				return 0, fmt.Errorf("the skill library is full (%d skills): delete one first", MaxSkills)
			}
		} else {
			if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM skills WHERE deleted = 0 AND sprig_id = ?", k.SprigID).Scan(&n); err != nil {
				return 0, err
			}
			if n >= MaxSprigSkills {
				return 0, fmt.Errorf("%s already has %d skills of its own: delete one first", k.SprigID, MaxSprigSkills)
			}
		}
		if deleted { // recreated: numbering carries on from the old versions
			// Old versions of a sprig's own skill stay that sprig's: nobody
			// else may take the id over (and with it, restore or history).
			var was sql.NullString
			if err := tx.QueryRowContext(ctx, "SELECT sprig_id FROM skills WHERE id = ?", k.ID).Scan(&was); err != nil {
				return 0, err
			}
			if was.Valid && was.String != k.SprigID {
				return 0, fmt.Errorf("%w: the id %q belonged to another sprig's skill; pick another", ErrSkillIDTaken, k.ID)
			}
			if _, err := tx.ExecContext(ctx, "UPDATE skills SET deleted = 0, sprig_id = ? WHERE id = ?", owner, k.ID); err != nil {
				return 0, err
			}
		} else if _, err := tx.ExecContext(ctx, "INSERT INTO skills (id, version, created_at, updated_at, sprig_id) VALUES (?, 0, ?, ?, ?)", k.ID, now, now, owner); err != nil {
			return 0, err
		}
	case err != nil:
		return 0, err
	case base != cur:
		return 0, &VersionConflict{Current: cur}
	default:
		var name, desc, body, in, check string
		if err := tx.QueryRowContext(ctx, "SELECT name, description, body, inputs, success_check FROM skill_versions WHERE skill_id = ? AND version = ?", k.ID, cur).Scan(&name, &desc, &body, &in, &check); err != nil {
			return 0, err
		}
		if name == k.Name && desc == k.Description && body == k.Body && in == inputs && check == k.SuccessCheck {
			return cur, ErrNoChange
		}
	}
	next := cur + 1
	var uid any
	if k.UserID != 0 {
		uid = k.UserID
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO skill_versions (skill_id, version, name, description, body, note, author, user_id, created_at, inputs, success_check) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		k.ID, next, k.Name, k.Description, k.Body, k.Note, k.Author, uid, now, inputs, k.SuccessCheck); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE skills SET version = ?, updated_at = ? WHERE id = ?", next, now, k.ID); err != nil {
		return 0, err
	}
	return next, tx.Commit()
}

// DeleteSkill removes a skill from the library. Its versions are kept, so
// a skill recreated with the same id continues the numbering.
func (s *Store) DeleteSkill(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, "UPDATE skills SET deleted = 1, updated_at = ? WHERE id = ? AND deleted = 0", s.unix(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// PromoteSkill moves a sprig's own skill into the shared library, where
// every sprig is offered it. It keeps its id and versions. The trail
// entry, if given, is written in the same transaction.
func (s *Store) PromoteSkill(ctx context.Context, id string, trail *TrailEntry) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var owner sql.NullString
	if err := tx.QueryRowContext(ctx, "SELECT sprig_id FROM skills WHERE id = ? AND deleted = 0", id).Scan(&owner); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if !owner.Valid {
		return fmt.Errorf("%s is already in the shared library", id)
	}
	var n int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM skills WHERE deleted = 0 AND sprig_id IS NULL").Scan(&n); err != nil {
		return err
	}
	if n >= MaxSkills {
		return fmt.Errorf("the skill library is full (%d skills): delete one first", MaxSkills)
	}
	if _, err := tx.ExecContext(ctx, "UPDATE skills SET sprig_id = NULL, updated_at = ? WHERE id = ?", s.unix(), id); err != nil {
		return err
	}
	if trail != nil {
		if _, err := tx.ExecContext(ctx, "INSERT INTO trail (at, user_id, character, kind, summary, detail, undo) VALUES (?, ?, ?, ?, ?, ?, ?)",
			s.unix(), nullInt(trail.UserID), trail.Character, trail.Kind, s.clean(trail.Summary), s.clean(trail.Detail), string(trail.Undo)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteSprigSkill deletes a skill only while it is still that sprig's
// own (a skill promoted meanwhile stays). ErrNotFound if it isn't.
func (s *Store) DeleteSprigSkill(ctx context.Context, id, sprig string) error {
	res, err := s.db.ExecContext(ctx, "UPDATE skills SET deleted = 1, updated_at = ? WHERE id = ? AND sprig_id = ? AND deleted = 0", s.unix(), id, sprig)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// --- skill runs ---

// Skill run outcomes. A run with no outcome is still going.
const (
	SkillSucceeded = "succeeded"
	SkillFailed    = "failed"
	SkillStopped   = "stopped" // the person stopped it
)

// SkillRun is one time a sprig followed a skill.
type SkillRun struct {
	ID         int64     `json:"id"`
	SkillID    string    `json:"skill_id"`
	Version    int       `json:"version"`
	SprigID    string    `json:"sprig_id"`
	UserID     int64     `json:"user_id,omitempty"`
	ChatRef    string    `json:"-"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
	Outcome    string    `json:"outcome,omitempty"`
	Note       string    `json:"note,omitempty"`
	ForcedAsk  bool      `json:"forced_ask,omitempty"`
}

// StartSkillRun records that a sprig began following a skill.
func (s *Store) StartSkillRun(ctx context.Context, r SkillRun) (int64, error) {
	var uid any
	if r.UserID != 0 {
		uid = r.UserID
	}
	res, err := s.db.ExecContext(ctx, "INSERT INTO skill_runs (skill_id, version, sprig_id, user_id, chat_ref, started_at, forced_ask) VALUES (?, ?, ?, ?, ?, ?, ?)",
		r.SkillID, r.Version, r.SprigID, uid, r.ChatRef, s.unix(), r.ForcedAsk)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// FinishSkillRun records how a sprig's run ended. Only a run that is
// still going, and is that sprig's, can be finished.
func (s *Store) FinishSkillRun(ctx context.Context, id int64, sprig, outcome, note string) error {
	switch outcome {
	case SkillSucceeded, SkillFailed, SkillStopped:
	default:
		return fmt.Errorf("outcome must be %s, %s or %s", SkillSucceeded, SkillFailed, SkillStopped)
	}
	res, err := s.db.ExecContext(ctx, "UPDATE skill_runs SET outcome = ?, note = ?, finished_at = ? WHERE id = ? AND sprig_id = ? AND outcome = ''",
		outcome, s.clean(note), s.unix(), id, sprig)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// StopSkillRuns marks a sprig's runs still going in a chat as stopped by
// the person, and returns how many there were.
func (s *Store) StopSkillRuns(ctx context.Context, sprig, chatRef string) (int, error) {
	res, err := s.db.ExecContext(ctx, "UPDATE skill_runs SET outcome = ?, finished_at = ? WHERE sprig_id = ? AND chat_ref = ? AND outcome = ''",
		SkillStopped, s.unix(), sprig, chatRef)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// SkillRunCount is how many times a sprig has started following a skill.
func (s *Store) SkillRunCount(ctx context.Context, skillID, sprig string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM skill_runs WHERE skill_id = ? AND sprig_id = ?", skillID, sprig).Scan(&n)
	return n, err
}

// ForcedRun is a run that asks first (SkillRun.ForcedAsk): which skill,
// and which of the sprig's runs of it this is (1 for the first).
type ForcedRun struct {
	SkillID string
	Number  int
}

// ForcedSkillRun returns the newest run a sprig is still following in a
// chat that asks first, started after since; ErrNotFound if none.
func (s *Store) ForcedSkillRun(ctx context.Context, sprig, chatRef string, since time.Time) (ForcedRun, error) {
	var r ForcedRun
	err := s.db.QueryRowContext(ctx, `SELECT r.skill_id, (SELECT COUNT(*) FROM skill_runs p WHERE p.skill_id = r.skill_id AND p.sprig_id = r.sprig_id AND p.id <= r.id)
		FROM skill_runs r WHERE r.sprig_id = ? AND r.chat_ref = ? AND r.outcome = '' AND r.forced_ask = 1 AND r.started_at >= ?
		ORDER BY r.id DESC LIMIT 1`, sprig, chatRef, since.Unix()).Scan(&r.SkillID, &r.Number)
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}

// SkillRuns lists a skill's runs, newest first (sprig "" = every sprig's).
func (s *Store) SkillRuns(ctx context.Context, skillID, sprig string, limit int) ([]SkillRun, error) {
	q := "SELECT id, skill_id, version, sprig_id, user_id, chat_ref, started_at, finished_at, outcome, note, forced_ask FROM skill_runs WHERE skill_id = ?"
	args := []any{skillID}
	if sprig != "" {
		q += " AND sprig_id = ?"
		args = append(args, sprig)
	}
	q += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SkillRun
	for rows.Next() {
		var r SkillRun
		var uid, fin sql.NullInt64
		var at int64
		if err := rows.Scan(&r.ID, &r.SkillID, &r.Version, &r.SprigID, &uid, &r.ChatRef, &at, &fin, &r.Outcome, &r.Note, &r.ForcedAsk); err != nil {
			return nil, err
		}
		r.UserID, r.StartedAt = uid.Int64, time.Unix(at, 0)
		if fin.Valid {
			r.FinishedAt = time.Unix(fin.Int64, 0)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
