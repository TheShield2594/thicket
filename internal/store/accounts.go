package store

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"time"
)

// ErrUsernameTaken means another user already has that username.
var ErrUsernameTaken = errors.New("that username is taken")

var usernameRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{1,31}$`)

// ValidUsername reports whether name can be a username: 2 to 32 letters,
// digits, dots, dashes and underscores, starting with a letter or digit.
func ValidUsername(name string) bool { return usernameRe.MatchString(name) }

// Login is a user's username and whether they have a password.
type Login struct {
	UserID      int64
	Username    string
	HasPassword bool
}

func isUnique(err error) bool { return err != nil && strings.Contains(err.Error(), "UNIQUE") }

// SetLogin gives a user a username and password hash (replacing any).
func (s *Store) SetLogin(ctx context.Context, userID int64, username, hash string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO logins (user_id, username, password_hash, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET username = excluded.username, password_hash = excluded.password_hash, updated_at = excluded.updated_at`,
		userID, username, hash, s.unix())
	if isUnique(err) {
		return ErrUsernameTaken
	}
	return err
}

// ChangePassword sets a user's login and, in the same transaction,
// deletes their app sign-in tokens except the one with hash keepToken (an
// app changing its own password), so neither happens without the other.
// It returns how many tokens were deleted.
func (s *Store) ChangePassword(ctx context.Context, userID int64, username, hash, keepToken string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO logins (user_id, username, password_hash, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET username = excluded.username, password_hash = excluded.password_hash, updated_at = excluded.updated_at`,
		userID, username, hash, s.unix())
	if isUnique(err) {
		return 0, ErrUsernameTaken
	}
	if err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, "DELETE FROM api_tokens WHERE user_id = ? AND kind = ? AND hash != ?",
		userID, TokenApp, keepToken)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, tx.Commit()
}

// LoginFor returns a user's login, or ErrNotFound.
func (s *Store) LoginFor(ctx context.Context, userID int64) (*Login, error) {
	var l Login
	var hash string
	err := s.db.QueryRowContext(ctx, "SELECT user_id, username, password_hash FROM logins WHERE user_id = ?", userID).Scan(&l.UserID, &l.Username, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	l.HasPassword = hash != ""
	return &l, err
}

// LoginByUsername returns the user with that username (any case) and
// their password hash ("" if they have none).
func (s *Store) LoginByUsername(ctx context.Context, username string) (*User, string, error) {
	var uid int64
	var hash string
	err := s.db.QueryRowContext(ctx, "SELECT user_id, password_hash FROM logins WHERE username = ?", username).Scan(&uid, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", err
	}
	u, err := s.UserByID(ctx, uid)
	return u, hash, err
}

// SetPassword changes a user's password hash.
func (s *Store) SetPassword(ctx context.Context, userID int64, hash string) error {
	res, err := s.db.ExecContext(ctx, "UPDATE logins SET password_hash = ?, updated_at = ? WHERE user_id = ?", hash, s.unix(), userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Invite is a one-time link to make (or give a login to) an account.
type Invite struct {
	UserID    int64 // an existing user to give a login; 0 makes a new one
	Role      string
	Name      string
	CreatedBy int64
	ExpiresAt time.Time
	// OwnSprig: help them make a private sprig of their own when they
	// arrive (adults only).
	OwnSprig bool
}

// CreateInvite stores an invite and returns its token.
func (s *Store) CreateInvite(ctx context.Context, inv Invite, ttl time.Duration) (string, error) {
	if inv.Role != RoleAdmin && inv.Role != RoleKid {
		inv.Role = RoleMember
	}
	token, hash := NewToken()
	now := s.unix()
	dbr, kid := dbRole(inv.Role)
	own := 0
	if inv.OwnSprig && inv.Role != RoleKid {
		own = 1
	}
	_, err := s.db.ExecContext(ctx, "INSERT INTO invites (token_hash, user_id, role, kid, name, created_by, expires_at, created_at, own_sprig) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
		hash, nullInt(inv.UserID), dbr, kid, inv.Name, nullInt(inv.CreatedBy), now+int64(ttl.Seconds()), now, own)
	return token, err
}

// InviteByToken returns a live (unused, unexpired) invite.
func (s *Store) InviteByToken(ctx context.Context, token string) (*Invite, error) {
	return s.inviteByHash(ctx, s.db, HashToken(token))
}

// InviteByHash is InviteByToken for a token's hash.
func (s *Store) InviteByHash(ctx context.Context, hash string) (*Invite, error) {
	return s.inviteByHash(ctx, s.db, hash)
}

type querier interface {
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
}

func (s *Store) inviteByHash(ctx context.Context, q querier, hash string) (*Invite, error) {
	var inv Invite
	var uid, by sql.NullInt64
	var exp int64
	var kid, own int
	err := q.QueryRowContext(ctx, "SELECT user_id, role, kid, name, created_by, expires_at, own_sprig FROM invites WHERE token_hash = ? AND used_at IS NULL AND expires_at > ?",
		hash, s.unix()).Scan(&uid, &inv.Role, &kid, &inv.Name, &by, &exp, &own)
	if kid == 1 {
		inv.Role = RoleKid
	}
	inv.OwnSprig = own == 1
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	inv.UserID, inv.CreatedBy, inv.ExpiresAt = uid.Int64, by.Int64, time.Unix(exp, 0)
	return &inv, err
}

// RedeemInvite uses an invite (by its token's hash), all in one
// transaction: the user it names, or a new web user with its role, gets
// the username and password hash (hash may be empty for an OIDC-only
// login, and username then too). It returns the user.
func (s *Store) RedeemInvite(ctx context.Context, tokenHash, username, hash string) (*User, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	inv, err := s.inviteByHash(ctx, tx, tokenHash)
	if err != nil {
		return nil, err
	}
	now := s.unix()
	if res, err := tx.ExecContext(ctx, "UPDATE invites SET used_at = ? WHERE token_hash = ? AND used_at IS NULL", now, tokenHash); err != nil {
		return nil, err
	} else if n, _ := res.RowsAffected(); n != 1 {
		return nil, ErrNotFound
	}
	uid := inv.UserID
	if uid == 0 {
		ext, _ := NewToken()
		if uid, err = s.insertInvited(ctx, tx, inv, "web", "w-"+ext[:16], now); err != nil {
			return nil, err
		}
	}
	if username != "" {
		_, err := tx.ExecContext(ctx, `INSERT INTO logins (user_id, username, password_hash, updated_at) VALUES (?, ?, ?, ?)
			ON CONFLICT(user_id) DO UPDATE SET username = excluded.username, password_hash = excluded.password_hash, updated_at = excluded.updated_at`,
			uid, username, hash, now)
		if isUnique(err) {
			return nil, ErrUsernameTaken
		}
		if err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.UserByID(ctx, uid)
}

// insertInvited makes the user an invite is for, with one identity.
func (s *Store) insertInvited(ctx context.Context, tx *sql.Tx, inv *Invite, channel, ext string, now int64) (int64, error) {
	dbr, kid := dbRole(inv.Role)
	own := 0
	if inv.OwnSprig && kid == 0 {
		own = 1
	}
	res, err := tx.ExecContext(ctx, "INSERT INTO users (channel, external_id, name, role, kid, created_at, timezone, own_sprig, invited_by) VALUES (?, ?, ?, ?, ?, ?, "+householdTimezone+", ?, ?)",
		channel, ext, inv.Name, dbr, kid, now, own, nullInt(inv.CreatedBy))
	if err != nil {
		return 0, err
	}
	uid, _ := res.LastInsertId()
	if _, err := tx.ExecContext(ctx, "INSERT INTO identities (user_id, channel, external_id, created_at) VALUES (?, ?, ?, ?)", uid, channel, ext, now); err != nil {
		return 0, err
	}
	return uid, nil
}

// RedeemInviteChat uses an invite from a chat app: the account that sent
// it (channel, ext) becomes the new person the invite is for. Invites that
// give an existing person a login can't be used this way. ErrIdentityTaken
// means that account already belongs to someone; the invite stays unused.
func (s *Store) RedeemInviteChat(ctx context.Context, tokenHash, channel, ext string) (*User, *Invite, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	inv, err := s.inviteByHash(ctx, tx, tokenHash)
	if err != nil {
		return nil, nil, err
	}
	if inv.UserID != 0 {
		return nil, nil, ErrNotFound
	}
	var taken int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM identities WHERE channel = ? AND external_id = ?", channel, ext).Scan(&taken); err != nil {
		return nil, nil, err
	}
	if taken > 0 {
		return nil, nil, ErrIdentityTaken
	}
	now := s.unix()
	if res, err := tx.ExecContext(ctx, "UPDATE invites SET used_at = ? WHERE token_hash = ? AND used_at IS NULL", now, tokenHash); err != nil {
		return nil, nil, err
	} else if n, _ := res.RowsAffected(); n != 1 {
		return nil, nil, ErrNotFound
	}
	uid, err := s.insertInvited(ctx, tx, inv, channel, ext, now)
	if err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	u, err := s.UserByID(ctx, uid)
	return u, inv, err
}

// --- OpenID Connect ---

// OIDCUser returns the user an OIDC account is linked to.
func (s *Store) OIDCUser(ctx context.Context, issuer, subject string) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, "SELECT "+userCols+" FROM users WHERE id = (SELECT user_id FROM oidc_identities WHERE issuer = ? AND subject = ?)", issuer, subject))
}

// LinkOIDC links an OIDC account to a user.
func (s *Store) LinkOIDC(ctx context.Context, issuer, subject string, userID int64, email string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO oidc_identities (issuer, subject, user_id, email, created_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(issuer, subject) DO UPDATE SET email = excluded.email`, issuer, subject, userID, email, s.unix())
	return err
}

// HasOIDC reports whether a user has a linked OIDC account.
func (s *Store) HasOIDC(ctx context.Context, userID int64) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM oidc_identities WHERE user_id = ?", userID).Scan(&n)
	return n > 0, err
}

// UserByEmail finds the one user with that email (any case).
func (s *Store) UserByEmail(ctx context.Context, email string) (*User, error) {
	if strings.TrimSpace(email) == "" {
		return nil, ErrNotFound
	}
	return scanUser(s.db.QueryRowContext(ctx, "SELECT "+userCols+" FROM users WHERE email = ? COLLATE NOCASE ORDER BY id LIMIT 1", email))
}

// SetUserEmail sets the email OIDC sign-ins are matched by.
func (s *Store) SetUserEmail(ctx context.Context, userID int64, email string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE users SET email = ? WHERE id = ?", strings.TrimSpace(email), userID)
	return err
}

// SyncUserRole sets a user's role from the sign-in provider's groups.
// The owner's role never changes this way.
func (s *Store) SyncUserRole(ctx context.Context, userID int64, role string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE users SET role = ? WHERE id = ? AND role != 'owner'", role, userID)
	return err
}

// CreateWebUser makes a user who signs in on the web only.
func (s *Store) CreateWebUser(ctx context.Context, name, role string) (*User, error) {
	ext, _ := NewToken()
	return s.CreateUser(ctx, "web", "w-"+ext[:16], name, role)
}

// SaveOIDCState remembers a sign-in started at the provider.
func (s *Store) SaveOIDCState(ctx context.Context, state, verifier, nonce, inviteHash string, ttl time.Duration) error {
	now := s.unix()
	_, _ = s.db.ExecContext(ctx, "DELETE FROM oidc_states WHERE expires_at < ?", now)
	_, err := s.db.ExecContext(ctx, "INSERT INTO oidc_states (state_hash, verifier, nonce, invite_hash, expires_at) VALUES (?, ?, ?, ?, ?)",
		HashToken(state), verifier, nonce, inviteHash, now+int64(ttl.Seconds()))
	return err
}

// TakeOIDCState returns and forgets a sign-in in progress (once).
func (s *Store) TakeOIDCState(ctx context.Context, state string) (verifier, nonce, inviteHash string, err error) {
	err = s.db.QueryRowContext(ctx, "DELETE FROM oidc_states WHERE state_hash = ? AND expires_at > ? RETURNING verifier, nonce, invite_hash",
		HashToken(state), s.unix()).Scan(&verifier, &nonce, &inviteHash)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return
}

// --- sessions ---

// WebSession is a signed-in browser, safe to show.
type WebSession struct {
	PublicID   string
	UserID     int64
	UserName   string
	UserAgent  string
	Method     string
	CreatedAt  time.Time
	LastSeenAt time.Time
	Current    bool
}

// WebSessions lists live sessions, newest activity first: one user's, or
// everyone's (userID 0). current is the caller's token, to mark it.
func (s *Store) WebSessions(ctx context.Context, userID int64, current string) ([]WebSession, error) {
	now := s.unix()
	q := `SELECT w.public_id, w.user_id, COALESCE(NULLIF(l.username, ''), NULLIF(u.name, ''), 'user ' || u.id), w.user_agent, w.method, w.created_at, w.last_seen_at, w.token_hash
		FROM web_sessions w JOIN users u ON u.id = w.user_id LEFT JOIN logins l ON l.user_id = w.user_id
		WHERE w.expires_at > ? AND (w.max_expires_at = 0 OR w.max_expires_at > ?)`
	args := []any{now, now}
	if userID != 0 {
		q += " AND w.user_id = ?"
		args = append(args, userID)
	}
	rows, err := s.db.QueryContext(ctx, q+" ORDER BY w.last_seen_at DESC LIMIT 200", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cur := ""
	if current != "" {
		cur = HashToken(current)
	}
	var out []WebSession
	for rows.Next() {
		var w WebSession
		var created, seen int64
		var hash string
		if err := rows.Scan(&w.PublicID, &w.UserID, &w.UserName, &w.UserAgent, &w.Method, &created, &seen, &hash); err != nil {
			return nil, err
		}
		w.CreatedAt, w.LastSeenAt, w.Current = time.Unix(created, 0), time.Unix(seen, 0), hash == cur
		out = append(out, w)
	}
	return out, rows.Err()
}

// RevokeWebSession ends a session by its public id; userID 0 can end
// anyone's (admins). It reports whether one was ended.
func (s *Store) RevokeWebSession(ctx context.Context, publicID string, userID int64) (bool, error) {
	if publicID == "" {
		return false, nil
	}
	q, args := "DELETE FROM web_sessions WHERE public_id = ?", []any{publicID}
	if userID != 0 {
		q += " AND user_id = ?"
		args = append(args, userID)
	}
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// RevokeUserSessions ends all of a user's sessions except the one with
// token keep (if given), returning how many ended.
func (s *Store) RevokeUserSessions(ctx context.Context, userID int64, keep string) (int64, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM web_sessions WHERE user_id = ? AND token_hash != ?", userID, HashToken(keep))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
