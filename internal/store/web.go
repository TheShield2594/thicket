package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"
)

// NewToken returns a random URL-safe token and the hash stored for it.
func NewToken() (token, hash string) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, HashToken(token)
}

func HashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// CreateWebLink stores a one-time sign-in link. userID 0 means "the owner,
// created on first use if there is none yet".
func (s *Store) CreateWebLink(ctx context.Context, userID int64, ttl time.Duration) (string, error) {
	token, hash := NewToken()
	var uid any
	if userID != 0 {
		uid = userID
	}
	now := s.unix()
	_, err := s.db.ExecContext(ctx, "INSERT INTO web_links (token_hash, user_id, expires_at, created_at) VALUES (?, ?, ?, ?)",
		hash, uid, now+int64(ttl.Seconds()), now)
	return token, err
}

// RedeemWebLink marks a link used and returns its user (0 = the owner).
// A link works once, before it expires.
func (s *Store) RedeemWebLink(ctx context.Context, token string) (int64, error) {
	now := s.unix()
	var uid sql.NullInt64
	err := s.db.QueryRowContext(ctx, `UPDATE web_links SET used_at = ? WHERE token_hash = ? AND used_at IS NULL AND expires_at > ?
		RETURNING user_id`, now, HashToken(token), now).Scan(&uid)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	_, _ = s.db.ExecContext(ctx, "DELETE FROM web_links WHERE expires_at < ?", now-86400)
	return uid.Int64, err
}

// Owner returns the owner, or ErrNotFound.
func (s *Store) Owner(ctx context.Context) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, "SELECT "+userCols+" FROM users WHERE role = 'owner' ORDER BY id LIMIT 1"))
}

// SessionMaxAge is how long a web session can last however active it
// stays: then the person signs in again.
const SessionMaxAge = 90 * 24 * time.Hour

func (s *Store) CreateWebSession(ctx context.Context, userID int64, userAgent string, ttl time.Duration) (string, error) {
	return s.CreateWebSessionVia(ctx, userID, userAgent, ttl, "link")
}

// CreateWebSessionVia starts a session, noting how the person signed in
// (link, password, oidc, invite).
func (s *Store) CreateWebSessionVia(ctx context.Context, userID int64, userAgent string, ttl time.Duration, method string) (string, error) {
	token, hash := NewToken()
	pub, _ := NewToken()
	if len(userAgent) > 200 {
		userAgent = userAgent[:200]
	}
	now := s.unix()
	_, err := s.db.ExecContext(ctx, `INSERT INTO web_sessions (token_hash, user_id, user_agent, expires_at, last_seen_at, created_at, public_id, method, max_expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		hash, userID, userAgent, now+int64(ttl.Seconds()), now, now, pub[:16], method, now+int64(SessionMaxAge.Seconds()))
	return token, err
}

// WebSessionUser returns the user for a live session and slides its expiry.
func (s *Store) WebSessionUser(ctx context.Context, token string, ttl time.Duration) (*User, error) {
	now := s.unix()
	var uid int64
	err := s.db.QueryRowContext(ctx, "SELECT user_id FROM web_sessions WHERE token_hash = ? AND expires_at > ? AND (max_expires_at = 0 OR max_expires_at > ?)",
		HashToken(token), now, now).Scan(&uid)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	// Refresh at most once a minute to keep writes rare.
	_, _ = s.db.ExecContext(ctx, "UPDATE web_sessions SET last_seen_at = ?, expires_at = ? WHERE token_hash = ? AND last_seen_at < ?",
		now, now+int64(ttl.Seconds()), HashToken(token), now-60)
	return s.UserByID(ctx, uid)
}

func (s *Store) DeleteWebSession(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM web_sessions WHERE token_hash = ?", HashToken(token))
	return err
}

// PruneWeb drops expired sessions and links, and web transcripts older
// than the message retention window.
func (s *Store) PruneWeb(ctx context.Context, retention time.Duration) error {
	now := s.unix()
	if _, err := s.db.ExecContext(ctx, "DELETE FROM web_sessions WHERE expires_at < ? OR (max_expires_at > 0 AND max_expires_at < ?)", now, now); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, "DELETE FROM web_links WHERE expires_at < ?", now); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, "DELETE FROM web_messages WHERE created_at < ?", now-int64(retention.Seconds()))
	return err
}

type WebMessage struct {
	ID        int64  `json:"id"`
	ChatID    string `json:"chat"`
	Role      string `json:"role"`
	Character string `json:"character,omitempty"`
	// Name is who the message was sent as, e.g. "Pippa (preview)" for a
	// sprig that isn't created yet.
	Name      string    `json:"name,omitempty"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"at"`
	// Pinned: the conversation message this is a copy of is pinned.
	Pinned bool `json:"pinned,omitempty"`
}

func (s *Store) AddWebMessage(ctx context.Context, userID int64, m WebMessage) (WebMessage, error) {
	now := s.now()
	m.Content = s.clean(m.Content) // the returned copy is what the browser gets
	res, err := s.db.ExecContext(ctx, "INSERT INTO web_messages (user_id, chat_id, role, character, name, content, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
		userID, m.ChatID, m.Role, m.Character, m.Name, m.Content, now.Unix())
	if err != nil {
		return m, err
	}
	m.ID, _ = res.LastInsertId()
	m.CreatedAt = time.Unix(now.Unix(), 0)
	return m, nil
}

func (s *Store) DeleteWebMessage(ctx context.Context, chatID string, id int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM web_messages WHERE chat_id = ? AND id = ?", chatID, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// webPinned is whether web message w's conversation copy is pinned.
const webPinned = `COALESCE((SELECT m.pinned FROM message_refs r JOIN messages m ON m.id = r.message_id
	WHERE r.chat_ref = 'web:' || w.chat_id AND r.platform_id = CAST(w.id AS TEXT)), 0)`

// WebMessageByID returns one message of a web chat.
func (s *Store) WebMessageByID(ctx context.Context, chatID string, id int64) (*WebMessage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, chat_id, role, character, name, content, created_at, `+webPinned+` FROM web_messages w
		WHERE chat_id = ? AND id = ?`, chatID, id)
	if err != nil {
		return nil, err
	}
	msgs, err := scanWebMessages(rows)
	if err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		return nil, ErrNotFound
	}
	return &msgs[0], nil
}

func scanWebMessages(rows *sql.Rows) ([]WebMessage, error) {
	defer rows.Close()
	var out []WebMessage
	for rows.Next() {
		var m WebMessage
		var at int64
		if err := rows.Scan(&m.ID, &m.ChatID, &m.Role, &m.Character, &m.Name, &m.Content, &at, &m.Pinned); err != nil {
			return nil, err
		}
		m.CreatedAt = time.Unix(at, 0)
		out = append(out, m)
	}
	return out, rows.Err()
}

// WebMessages returns up to limit messages before beforeID (0 = newest),
// oldest first.
func (s *Store) WebMessages(ctx context.Context, chatID string, beforeID int64, limit int) ([]WebMessage, error) {
	if beforeID <= 0 {
		beforeID = 1<<62 - 1
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, chat_id, role, character, name, content, created_at, `+webPinned+` FROM
		(SELECT * FROM web_messages WHERE chat_id = ? AND id < ? ORDER BY id DESC LIMIT ?) w ORDER BY id`, chatID, beforeID, limit)
	if err != nil {
		return nil, err
	}
	return scanWebMessages(rows)
}

// LatestWebMessages returns the newest message in each of the user's chats.
func (s *Store) LatestWebMessages(ctx context.Context, userID int64) ([]WebMessage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, chat_id, role, character, name, content, created_at, `+webPinned+` FROM web_messages w
		WHERE id IN (SELECT MAX(id) FROM web_messages WHERE user_id = ? GROUP BY chat_id)`, userID)
	if err != nil {
		return nil, err
	}
	return scanWebMessages(rows)
}

// CountMemories counts a user's live memories per character scope
// (” = shared).
func (s *Store) CountMemories(ctx context.Context, userID int64) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT character, COUNT(*) FROM memories WHERE user_id = ?
		AND (expires_at IS NULL OR expires_at > ?) GROUP BY character`, userID, s.unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var c string
		var n int
		if err := rows.Scan(&c, &n); err != nil {
			return nil, err
		}
		out[c] = n
	}
	return out, rows.Err()
}
