package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Permission levels, lowest first. What each allows is decided in the
// agent package; the store only keeps them.
const (
	LevelRead  = "read"
	LevelDraft = "draft"
	LevelAsk   = "ask"
	LevelAct   = "act"
)

// AllSprigs is the character value of a level that applies to every sprig.
const AllSprigs = "*"

// ValidLevel reports whether s is one of read, draft, ask, act.
func ValidLevel(s string) bool {
	switch s {
	case LevelRead, LevelDraft, LevelAsk, LevelAct:
		return true
	}
	return false
}

// ToolLevel returns the level set for a sprig and tool: the sprig's own,
// else the one for every sprig, else "" (use the tool's default).
func (s *Store) ToolLevel(ctx context.Context, character, tool string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT level FROM tool_levels WHERE tool = ? AND character IN (?, '*')
		ORDER BY character = '*' LIMIT 1`, tool, character).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetToolLevel sets a level; an empty level removes it. character is a
// sprig id or AllSprigs.
func (s *Store) SetToolLevel(ctx context.Context, character, tool, level string) error {
	if level == "" {
		_, err := s.db.ExecContext(ctx, "DELETE FROM tool_levels WHERE character = ? AND tool = ?", character, tool)
		return err
	}
	if !ValidLevel(level) {
		return errors.New("level must be read, draft, ask or act")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO tool_levels (character, tool, level, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(character, tool) DO UPDATE SET level = excluded.level, updated_at = excluded.updated_at`, character, tool, level, s.unix())
	return err
}

type ToolLevelRow struct {
	Character string
	Tool      string
	Level     string
	UpdatedAt time.Time
}

// ToolLevels lists every level that has been set.
func (s *Store) ToolLevels(ctx context.Context) ([]ToolLevelRow, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT character, tool, level, updated_at FROM tool_levels ORDER BY character, tool")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ToolLevelRow
	for rows.Next() {
		var r ToolLevelRow
		var at int64
		if err := rows.Scan(&r.Character, &r.Tool, &r.Level, &at); err != nil {
			return nil, err
		}
		r.UpdatedAt = time.Unix(at, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}
