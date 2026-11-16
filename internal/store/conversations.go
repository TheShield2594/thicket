package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type Conversation struct {
	ID             int64
	UserID         int64
	ChatRef        string
	StartedAt      time.Time
	LastActivityAt time.Time
	ReviewedAt     time.Time
	ReviewAttempts int
}

type Message struct {
	ID             int64
	ConversationID int64
	Role           string // user | assistant | event
	Character      string
	Content        string
	CreatedAt      time.Time
}

// OpenConversation returns the user's current conversation in a chat, or
// starts a new one if the last one went idle. Starting fresh after idle is
// what keeps old conversations from bleeding into new tasks.
func (s *Store) OpenConversation(ctx context.Context, userID int64, chatRef string, idle time.Duration) (*Conversation, error) {
	var c Conversation
	var started, last int64
	var reviewed sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT id, user_id, chat_ref, started_at, last_activity_at, reviewed_at, review_attempts
		FROM conversations WHERE user_id = ? AND chat_ref = ? ORDER BY id DESC LIMIT 1`, userID, chatRef).
		Scan(&c.ID, &c.UserID, &c.ChatRef, &started, &last, &reviewed, &c.ReviewAttempts)
	now := s.unix()
	if err == nil && !reviewed.Valid && now-last < int64(idle.Seconds()) {
		c.StartedAt, c.LastActivityAt = time.Unix(started, 0), time.Unix(last, 0)
		return &c, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	res, err := s.db.ExecContext(ctx, "INSERT INTO conversations (user_id, chat_ref, started_at, last_activity_at) VALUES (?, ?, ?, ?)", userID, chatRef, now, now)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &Conversation{ID: id, UserID: userID, ChatRef: chatRef, StartedAt: time.Unix(now, 0), LastActivityAt: time.Unix(now, 0)}, nil
}

func (s *Store) AddMessage(ctx context.Context, convID int64, role, character, content string) error {
	now := s.unix()
	if _, err := s.db.ExecContext(ctx, "INSERT INTO messages (conversation_id, role, character, content, created_at) VALUES (?, ?, ?, ?, ?)",
		convID, role, character, content, now); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, "UPDATE conversations SET last_activity_at = ? WHERE id = ?", now, convID)
	return err
}

// RecentMessages returns up to limit most recent messages, oldest first.
func (s *Store) RecentMessages(ctx context.Context, convID int64, limit int) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, conversation_id, role, character, content, created_at FROM
		(SELECT * FROM messages WHERE conversation_id = ? ORDER BY id DESC LIMIT ?) ORDER BY id`, convID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		var at int64
		if err := rows.Scan(&m.ID, &m.ConversationID, &m.Role, &m.Character, &m.Content, &at); err != nil {
			return nil, err
		}
		m.CreatedAt = time.Unix(at, 0)
		out = append(out, m)
	}
	return out, rows.Err()
}

// ConversationsToReview returns idle, unreviewed conversations that contain
// at least one user message. Conversations that failed review 3 times are
// skipped so a poison conversation can't burn budget forever.
func (s *Store) ConversationsToReview(ctx context.Context, idle time.Duration, limit int) ([]Conversation, error) {
	cutoff := s.unix() - int64(idle.Seconds())
	rows, err := s.db.QueryContext(ctx, `SELECT c.id, c.user_id, c.chat_ref, c.started_at, c.last_activity_at, c.review_attempts
		FROM conversations c WHERE c.reviewed_at IS NULL AND c.last_activity_at < ? AND c.review_attempts < 3
		AND EXISTS (SELECT 1 FROM messages m WHERE m.conversation_id = c.id AND m.role = 'user')
		ORDER BY c.last_activity_at LIMIT ?`, cutoff, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Conversation
	for rows.Next() {
		var c Conversation
		var st, la int64
		if err := rows.Scan(&c.ID, &c.UserID, &c.ChatRef, &st, &la, &c.ReviewAttempts); err != nil {
			return nil, err
		}
		c.StartedAt, c.LastActivityAt = time.Unix(st, 0), time.Unix(la, 0)
		out = append(out, c)
	}
	return out, rows.Err()
}

// MarkReviewed closes a conversation once memory review has run. If a
// message arrived after the reviewed snapshot (asOf), it stays open so the
// whole conversation is reviewed again once it goes idle.
func (s *Store) MarkReviewed(ctx context.Context, convID int64, asOf time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, "UPDATE conversations SET reviewed_at = ? WHERE id = ? AND last_activity_at <= ?", s.unix(), convID, asOf.Unix())
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

func (s *Store) MarkReviewFailed(ctx context.Context, convID int64) error {
	_, err := s.db.ExecContext(ctx, "UPDATE conversations SET review_attempts = review_attempts + 1 WHERE id = ?", convID)
	return err
}

// PruneMessages deletes conversations (and their messages) older than the
// retention window once memory review is done with them. Durable facts live on in memories.
func (s *Store) PruneMessages(ctx context.Context, retention time.Duration) (int64, error) {
	cutoff := s.unix() - int64(retention.Seconds())
	// Also prune conversations review will never close (gave up after 3
	// failures, or nothing the user said), so retention holds for them too.
	res, err := s.db.ExecContext(ctx, `DELETE FROM conversations WHERE last_activity_at < ? AND (
		reviewed_at IS NOT NULL OR review_attempts >= 3
		OR NOT EXISTS (SELECT 1 FROM messages m WHERE m.conversation_id = conversations.id AND m.role = 'user'))`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// LastUserMessageAt is the last time a user wrote anything anywhere.
func (s *Store) LastUserMessageAt(ctx context.Context, userID int64) (time.Time, error) {
	var at sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT MAX(m.created_at) FROM messages m JOIN conversations c ON c.id = m.conversation_id
		WHERE c.user_id = ? AND m.role = 'user'`, userID).Scan(&at)
	return timeOf(at), err
}

// LastChatRef returns the chat the user most recently talked in, if any.
func (s *Store) LastChatRef(ctx context.Context, userID int64) (string, error) {
	var ref string
	err := s.db.QueryRowContext(ctx, "SELECT chat_ref FROM conversations WHERE user_id = ? ORDER BY last_activity_at DESC LIMIT 1", userID).Scan(&ref)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return ref, err
}
