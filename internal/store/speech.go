package store

import (
	"context"
	"database/sql"
	"errors"
)

// Speech modes: how a sprig answers someone out loud.
const (
	SpeechBrief = "brief" // a short spoken summary; the full text alongside
	SpeechFull  = "full"  // the whole reply read aloud (the default)
	SpeechOff   = "off"   // never out loud, whatever the sprig's file says
)

// SpeechMode is how a person likes a sprig to speak to them (SpeechFull
// if they never said).
func (s *Store) SpeechMode(ctx context.Context, userID int64, character string) (string, error) {
	var mode string
	err := s.db.QueryRowContext(ctx, "SELECT mode FROM speech_prefs WHERE user_id = ? AND character = ?", userID, character).Scan(&mode)
	if errors.Is(err, sql.ErrNoRows) {
		return SpeechFull, nil
	}
	return mode, err
}

// SetSpeechMode stores how a person likes a sprig to speak to them.
func (s *Store) SetSpeechMode(ctx context.Context, userID int64, character, mode string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO speech_prefs (user_id, character, mode, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (user_id, character) DO UPDATE SET mode = excluded.mode, updated_at = excluded.updated_at`,
		userID, character, mode, s.unix())
	return err
}

// AddWebAudio stores the spoken audio of a web message.
func (s *Store) AddWebAudio(ctx context.Context, messageID int64, contentType string, data []byte) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO web_audio (message_id, content_type, data) VALUES (?, ?, ?)
		ON CONFLICT (message_id) DO UPDATE SET content_type = excluded.content_type, data = excluded.data`, messageID, contentType, data)
	return err
}

// WebAudio returns the spoken audio of a message in a web chat.
func (s *Store) WebAudio(ctx context.Context, chatID string, messageID int64) (data []byte, contentType string, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT a.data, a.content_type FROM web_audio a JOIN web_messages w ON w.id = a.message_id
		WHERE w.chat_id = ? AND a.message_id = ?`, chatID, messageID).Scan(&data, &contentType)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrNotFound
	}
	return data, contentType, err
}
