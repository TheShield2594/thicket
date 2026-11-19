package store

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"time"
)

// --- kid sprigs: what parents see (agent/kidlimits.go) ---

// How a parent wants the report on their kids' sprigs.
const (
	KidReportDaily  = "daily"
	KidReportWeekly = "weekly" // in the weekly digest
	KidReportOff    = "off"
)

// ValidKidReport reports whether mode is one a parent can choose.
func ValidKidReport(mode string) bool {
	return mode == KidReportDaily || mode == KidReportWeekly || mode == KidReportOff
}

// KidReportMode is how a parent wants the kid report (weekly if they
// never said).
func (s *Store) KidReportMode(ctx context.Context, userID int64) (string, error) {
	var mode string
	err := s.db.QueryRowContext(ctx, "SELECT kids FROM digest_prefs WHERE user_id = ?", userID).Scan(&mode)
	if errors.Is(err, sql.ErrNoRows) {
		return KidReportWeekly, nil
	}
	return mode, err
}

// SetKidReportMode stores how a parent wants the kid report, keeping
// their weekly digest setting.
func (s *Store) SetKidReportMode(ctx context.Context, userID int64, mode string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO digest_prefs (user_id, mode, kids, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (user_id) DO UPDATE SET kids = excluded.kids, updated_at = excluded.updated_at`, userID, DigestAlways, mode, s.unix())
	return err
}

// KidMessagesBetween lists a kid's messages to a kid sprig and its replies
// in [from, to), oldest first. A kid's message counts for the sprig that
// answered it (the next reply in that conversation).
func (s *Store) KidMessagesBetween(ctx context.Context, kid int64, character string, from, to time.Time) ([]KidChatMessage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT m.id, m.conversation_id, m.role, m.character, m.content, m.created_at
		FROM messages m JOIN conversations c ON c.id = m.conversation_id
		WHERE c.user_id = ? AND m.role IN ('user', 'assistant') AND m.created_at >= ? AND m.created_at < ?
		ORDER BY m.conversation_id, m.id LIMIT 20000`, kid, from.Unix(), to.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var all []KidChatMessage
	for rows.Next() {
		var m KidChatMessage
		var at int64
		if err := rows.Scan(&m.ID, &m.ConversationID, &m.Role, &m.Character, &m.Content, &at); err != nil {
			return nil, err
		}
		m.CreatedAt, m.KidID = time.Unix(at, 0), kid
		all = append(all, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Each kid message goes to the sprig whose reply came next.
	var out []KidChatMessage
	var pending []KidChatMessage
	conv := int64(0)
	for _, m := range all {
		if m.ConversationID != conv {
			conv, pending = m.ConversationID, nil
		}
		if m.Role == "user" {
			pending = append(pending, m)
			continue
		}
		if m.Character == character {
			out = append(out, pending...)
			out = append(out, m)
		}
		pending = nil
	}
	slices.SortStableFunc(out, func(a, b KidChatMessage) int { return a.CreatedAt.Compare(b.CreatedAt) })
	return out, nil
}
