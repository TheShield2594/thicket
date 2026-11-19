package store

import (
	"context"
	"database/sql"
	"errors"
)

// AddRememberAcks records the chat app's ids for the confirmation of a
// "remember this", so a "forget this" in reply to it finds the memory.
func (s *Store) AddRememberAcks(ctx context.Context, chatRef string, memoryID, userID int64, platformIDs ...string) error {
	for _, pid := range platformIDs {
		if pid == "" {
			continue
		}
		if _, err := s.db.ExecContext(ctx, "INSERT OR REPLACE INTO remember_acks (chat_ref, platform_id, memory_id, user_id, created_at) VALUES (?, ?, ?, ?, ?)",
			chatRef, pid, memoryID, userID, s.unix()); err != nil {
			return err
		}
	}
	return nil
}

// RememberAck returns the memory a confirmation was for, and whose it is.
func (s *Store) RememberAck(ctx context.Context, chatRef, platformID string) (memoryID, userID int64, err error) {
	err = s.db.QueryRowContext(ctx, "SELECT memory_id, user_id FROM remember_acks WHERE chat_ref = ? AND platform_id = ?", chatRef, platformID).
		Scan(&memoryID, &userID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, ErrNotFound
	}
	return memoryID, userID, err
}

// SetMemoryHousehold moves a memory between the person's own memory and
// family memory (the answer to "just for you, or for the family?").
func (s *Store) SetMemoryHousehold(ctx context.Context, id int64, household bool) error {
	_, err := s.db.ExecContext(ctx, "UPDATE memories SET household = ?, updated_at = ? WHERE id = ?", boolInt(household), s.unix(), id)
	return err
}

// Where "remember this" goes in family sprig chats.
const (
	RememberUnset   = ""
	RememberPrivate = "private"
	RememberFamily  = "family"
)

// RememberScope is the person's answer to "just for you, or for the
// family?", or RememberUnset.
func (s *Store) RememberScope(ctx context.Context, userID int64) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, "SELECT remember_scope FROM users WHERE id = ?", userID).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return v, err
}

func (s *Store) SetRememberScope(ctx context.Context, userID int64, scope string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE users SET remember_scope = ? WHERE id = ?", scope, userID)
	return err
}
