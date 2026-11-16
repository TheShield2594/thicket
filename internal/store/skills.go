package store

import (
	"context"
	"database/sql"
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
}

// MaxSkills keeps the library (and the index in every prompt) small.
const MaxSkills = 100

var ErrNoChange = errors.New("nothing changed")

// VersionConflict means the skill changed since the caller read it.
type VersionConflict struct{ Current int }

func (e *VersionConflict) Error() string {
	return fmt.Sprintf("the skill changed since you read it (now version %d): read it again and redo your change on top", e.Current)
}

const skillCols = "s.id, v.version, v.name, v.description, v.body, v.note, v.author, v.user_id, v.created_at"

func scanSkill(r interface{ Scan(...any) error }) (*Skill, error) {
	var k Skill
	var uid sql.NullInt64
	var at int64
	if err := r.Scan(&k.ID, &k.Version, &k.Name, &k.Description, &k.Body, &k.Note, &k.Author, &uid, &at); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	k.UserID, k.CreatedAt = uid.Int64, time.Unix(at, 0)
	return &k, nil
}

// Skills returns the current version of every skill, by id.
func (s *Store) Skills(ctx context.Context) ([]Skill, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+skillCols+" FROM skills s JOIN skill_versions v ON v.skill_id = s.id AND v.version = s.version ORDER BY s.id")
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

// SkillByID returns a skill's current version, or version v if v > 0.
func (s *Store) SkillByID(ctx context.Context, id string, v int) (*Skill, error) {
	if v > 0 {
		return scanSkill(s.db.QueryRowContext(ctx, "SELECT "+skillCols+" FROM skills s JOIN skill_versions v ON v.skill_id = s.id WHERE s.id = ? AND v.version = ?", id, v))
	}
	return scanSkill(s.db.QueryRowContext(ctx, "SELECT "+skillCols+" FROM skills s JOIN skill_versions v ON v.skill_id = s.id AND v.version = s.version WHERE s.id = ?", id))
}

// SkillHistory lists a skill's versions, newest first (bodies included).
func (s *Store) SkillHistory(ctx context.Context, id string) ([]Skill, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+skillCols+" FROM skills s JOIN skill_versions v ON v.skill_id = s.id WHERE s.id = ? ORDER BY v.version DESC", id)
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
	err = tx.QueryRowContext(ctx, "SELECT version FROM skills WHERE id = ?", k.ID).Scan(&cur)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if base != 0 {
			return 0, ErrNotFound
		}
		var n int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM skills").Scan(&n); err != nil {
			return 0, err
		}
		if n >= MaxSkills {
			return 0, fmt.Errorf("the skill library is full (%d skills): delete one first", MaxSkills)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO skills (id, version, created_at, updated_at) VALUES (?, 0, ?, ?)", k.ID, now, now); err != nil {
			return 0, err
		}
	case err != nil:
		return 0, err
	case base != cur:
		return 0, &VersionConflict{Current: cur}
	default:
		var name, desc, body string
		if err := tx.QueryRowContext(ctx, "SELECT name, description, body FROM skill_versions WHERE skill_id = ? AND version = ?", k.ID, cur).Scan(&name, &desc, &body); err != nil {
			return 0, err
		}
		if name == k.Name && desc == k.Description && body == k.Body {
			return cur, ErrNoChange
		}
	}
	next := cur + 1
	var uid any
	if k.UserID != 0 {
		uid = k.UserID
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO skill_versions (skill_id, version, name, description, body, note, author, user_id, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
		k.ID, next, k.Name, k.Description, k.Body, k.Note, k.Author, uid, now); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE skills SET version = ?, updated_at = ? WHERE id = ?", next, now, k.ID); err != nil {
		return 0, err
	}
	return next, tx.Commit()
}

func (s *Store) DeleteSkill(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM skills WHERE id = ?", id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
