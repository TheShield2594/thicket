package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// MaxPinsPerConversation bounds how many messages one conversation can pin.
const MaxPinsPerConversation = 10

// AddMessageRefs records the chat app's ids for a stored message.
func (s *Store) AddMessageRefs(ctx context.Context, msgID int64, chatRef string, platformIDs ...string) error {
	for _, pid := range platformIDs {
		if pid == "" {
			continue
		}
		if _, err := s.db.ExecContext(ctx, "INSERT OR REPLACE INTO message_refs (chat_ref, platform_id, message_id) VALUES (?, ?, ?)",
			chatRef, pid, msgID); err != nil {
			return err
		}
	}
	return nil
}

// MessageRefs returns the chat app's ids for a stored message.
func (s *Store) MessageRefs(ctx context.Context, msgID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT platform_id FROM message_refs WHERE message_id = ? ORDER BY rowid", msgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// MessageOwner is a stored message with the conversation it belongs to.
type MessageOwner struct {
	Message
	UserID      int64
	SummaryUpto int64 // the conversation's; the message is summarised if its id is at or below
}

// Summarised reports whether the message was already folded into its
// conversation's summary.
func (m MessageOwner) Summarised() bool { return m.ID <= m.SummaryUpto }

// MessageByRef finds the stored message a chat app's message id belongs to.
func (s *Store) MessageByRef(ctx context.Context, chatRef, platformID string) (*MessageOwner, error) {
	var m MessageOwner
	var at int64
	err := s.db.QueryRowContext(ctx, `SELECT m.id, m.conversation_id, m.role, m.character, m.content, m.created_at, m.pinned, c.user_id, c.summary_upto
		FROM message_refs r JOIN messages m ON m.id = r.message_id JOIN conversations c ON c.id = m.conversation_id
		WHERE r.chat_ref = ? AND r.platform_id = ?`, chatRef, platformID).
		Scan(&m.ID, &m.ConversationID, &m.Role, &m.Character, &m.Content, &at, &m.Pinned, &m.UserID, &m.SummaryUpto)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	m.CreatedAt = time.Unix(at, 0)
	return &m, nil
}

// LastMessage returns the newest message of a conversation with one of
// the given roles (any role if none), or ErrNotFound.
func (s *Store) LastMessage(ctx context.Context, convID int64, roles ...string) (*Message, error) {
	q := "SELECT " + messageCols + " FROM messages WHERE conversation_id = ?"
	args := []any{convID}
	if len(roles) > 0 {
		q += " AND role IN ("
		for i, r := range roles {
			if i > 0 {
				q += ", "
			}
			q += "?"
			args = append(args, r)
		}
		q += ")"
	}
	msgs, err := scanMessages(s.db.QueryContext(ctx, q+" ORDER BY id DESC LIMIT 1", args...))
	if err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		return nil, ErrNotFound
	}
	return &msgs[0], nil
}

// DeleteMessage removes a message from its conversation's history (and
// with it, from what memory review will read).
func (s *Store) DeleteMessage(ctx context.Context, msgID int64) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM messages WHERE id = ?", msgID)
	return err
}

// SetPinned pins or unpins a message.
func (s *Store) SetPinned(ctx context.Context, msgID int64, pinned bool) error {
	v := 0
	if pinned {
		v = 1
	}
	_, err := s.db.ExecContext(ctx, "UPDATE messages SET pinned = ? WHERE id = ?", v, msgID)
	return err
}

// UnpinAll unpins every message of a conversation, returning how many.
func (s *Store) UnpinAll(ctx context.Context, convID int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, "UPDATE messages SET pinned = 0 WHERE conversation_id = ? AND pinned = 1", convID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PinnedMessages returns a conversation's pinned messages, oldest first.
func (s *Store) PinnedMessages(ctx context.Context, convID int64) ([]Message, error) {
	return scanMessages(s.db.QueryContext(ctx, "SELECT "+messageCols+" FROM messages WHERE conversation_id = ? AND pinned = 1 ORDER BY id", convID))
}

// CurrentConversation returns the user's open conversation in a chat
// without starting one, or ErrNotFound if it went idle or was closed.
func (s *Store) CurrentConversation(ctx context.Context, userID int64, chatRef string, idle time.Duration) (*Conversation, error) {
	var c Conversation
	var started, last int64
	var reviewed sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT id, user_id, chat_ref, started_at, last_activity_at, reviewed_at, review_attempts, summary, summary_upto
		FROM conversations WHERE user_id = ? AND chat_ref = ? ORDER BY id DESC LIMIT 1`, userID, chatRef).
		Scan(&c.ID, &c.UserID, &c.ChatRef, &started, &last, &reviewed, &c.ReviewAttempts, &c.Summary, &c.SummaryUpto)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if reviewed.Valid || s.unix()-last >= int64(idle.Seconds()) {
		return nil, ErrNotFound
	}
	c.StartedAt, c.LastActivityAt = time.Unix(started, 0), time.Unix(last, 0)
	return &c, nil
}
