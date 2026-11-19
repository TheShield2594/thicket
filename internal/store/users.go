package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type User struct {
	ID          int64
	Channel     string
	ExternalID  string
	Name        string
	Role        string
	Timezone    string
	OnboardedAt time.Time
	LastSeenAt  time.Time
	CreatedAt   time.Time
	// Email matches OIDC sign-ins to this user (verified emails only).
	Email string
}

// Household roles. Owners and admins are household admins; members are
// adults; kids talk only to the kid sprigs set up for them.
const (
	RoleOwner  = "owner"
	RoleAdmin  = "admin"
	RoleMember = "member"
	RoleKid    = "kid"
)

func (u *User) IsAdmin() bool { return u.Role == RoleOwner || u.Role == RoleAdmin }

// IsKid reports whether the user is one of the household's kids.
func (u *User) IsKid() bool { return u != nil && u.Role == RoleKid }

// PersonID and Child let characters decide who may use them.
func (u *User) PersonID() int64 { return u.ID }
func (u *User) Child() bool     { return u.IsKid() }

// ValidRole reports whether role is one a user can have.
func ValidRole(role string) bool {
	switch role {
	case RoleOwner, RoleAdmin, RoleMember, RoleKid:
		return true
	}
	return false
}

// dbRole maps a role to the stored role and kid flag (the role column
// predates kids, who are stored as members with kid = 1).
func dbRole(role string) (string, int) {
	if role == RoleKid {
		return RoleMember, 1
	}
	return role, 0
}

// Location returns the user's timezone, falling back to UTC.
func (u *User) Location() *time.Location {
	if u.Timezone != "" {
		if loc, err := time.LoadLocation(u.Timezone); err == nil {
			return loc
		}
	}
	return time.UTC
}

const userCols = "id, channel, external_id, name, role, timezone, onboarded_at, last_seen_at, created_at, email, kid"

func scanUser(r interface{ Scan(...any) error }) (*User, error) {
	var u User
	var onb, seen sql.NullInt64
	var created int64
	var kid int
	if err := r.Scan(&u.ID, &u.Channel, &u.ExternalID, &u.Name, &u.Role, &u.Timezone, &onb, &seen, &created, &u.Email, &kid); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	u.OnboardedAt, u.LastSeenAt, u.CreatedAt = timeOf(onb), timeOf(seen), time.Unix(created, 0)
	if kid == 1 {
		u.Role = RoleKid
	}
	return &u, nil
}

// UserByExternal finds the user a platform account belongs to.
func (s *Store) UserByExternal(ctx context.Context, channel, ext string) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, "SELECT "+userCols+" FROM users WHERE id = (SELECT user_id FROM identities WHERE channel = ? AND external_id = ?)", channel, ext))
}

func (s *Store) UserByID(ctx context.Context, id int64) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, "SELECT "+userCols+" FROM users WHERE id = ?", id))
}

func (s *Store) CreateUser(ctx context.Context, channel, ext, name, role string) (*User, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := s.unix()
	dbr, kid := dbRole(role)
	res, err := tx.ExecContext(ctx, "INSERT INTO users (channel, external_id, name, role, kid, created_at) VALUES (?, ?, ?, ?, ?, ?)",
		channel, ext, name, dbr, kid, now)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	if _, err := tx.ExecContext(ctx, "INSERT INTO identities (user_id, channel, external_id, created_at) VALUES (?, ?, ?, ?)", id, channel, ext, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.UserByID(ctx, id)
}

// Identity is one platform account of a user.
type Identity struct {
	Channel    string
	ExternalID string
}

func (s *Store) Identities(ctx context.Context, userID int64) ([]Identity, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT channel, external_id FROM identities WHERE user_id = ? ORDER BY id", userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Identity
	for rows.Next() {
		var i Identity
		if err := rows.Scan(&i.Channel, &i.ExternalID); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// ErrIdentityTaken means the account already belongs to a user with a
// history of their own, which linking won't merge.
var ErrIdentityTaken = errors.New("that account already has its own Thicket profile")

// LinkIdentity attaches a platform account to userID. An account that
// already belongs to another user is moved only if that user has nothing
// to lose (just this one account, no memories, no jobs, not an owner);
// that profile is then deleted. Otherwise ErrIdentityTaken. Linking never
// changes anyone's role.
func (s *Store) LinkIdentity(ctx context.Context, userID int64, channel, ext string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var other int64
	err = tx.QueryRowContext(ctx, "SELECT user_id FROM identities WHERE channel = ? AND external_id = ?", channel, ext).Scan(&other)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return err
	case other == userID:
		return nil
	default:
		var ids, mems, jobs int
		var role string
		if err := tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM identities WHERE user_id = ?), (SELECT COUNT(*) FROM memories WHERE user_id = ?),
			(SELECT COUNT(*) FROM jobs WHERE user_id = ?), (SELECT role FROM users WHERE id = ?)`, other, other, other, other).Scan(&ids, &mems, &jobs, &role); err != nil {
			return err
		}
		if ids > 1 || mems > 0 || jobs > 0 || role == "owner" {
			return ErrIdentityTaken
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM users WHERE id = ?", other); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO identities (user_id, channel, external_id, created_at) VALUES (?, ?, ?, ?)", userID, channel, ext, s.unix()); err != nil {
		return err
	}
	return tx.Commit()
}

// UseIdentity records that the user just wrote from this account and
// makes it the one proactive messages go to.
func (s *Store) UseIdentity(ctx context.Context, userID int64, channel, ext string) error {
	now := s.unix()
	if _, err := s.db.ExecContext(ctx, "UPDATE identities SET last_seen_at = ? WHERE user_id = ? AND channel = ? AND external_id = ?", now, userID, channel, ext); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, "UPDATE users SET channel = ?, external_id = ?, last_seen_at = ? WHERE id = ? AND (channel != ? OR external_id != ?)",
		channel, ext, now, userID, channel, ext)
	return err
}

func (s *Store) HasOwner(ctx context.Context) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM users WHERE role = 'owner'").Scan(&n)
	return n > 0, err
}

func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+userCols+" FROM users ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

// Admins returns owners and admins, who receive failure reports.
func (s *Store) Admins(ctx context.Context) ([]User, error) {
	all, err := s.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	var out []User
	for _, u := range all {
		if u.IsAdmin() {
			out = append(out, u)
		}
	}
	return out, nil
}

func (s *Store) UpdateUserProfile(ctx context.Context, id int64, name, tz string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE users SET name = COALESCE(NULLIF(?, ''), name), timezone = COALESCE(NULLIF(?, ''), timezone) WHERE id = ?", name, tz, id)
	return err
}

func (s *Store) SetUserRole(ctx context.Context, id int64, role string) error {
	if !ValidRole(role) {
		return fmt.Errorf("unknown role %q", role)
	}
	dbr, kid := dbRole(role)
	_, err := s.db.ExecContext(ctx, "UPDATE users SET role = ?, kid = ? WHERE id = ?", dbr, kid, id)
	return err
}

func (s *Store) MarkOnboarded(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, "UPDATE users SET onboarded_at = COALESCE(onboarded_at, ?) WHERE id = ?", s.unix(), id)
	return err
}

func (s *Store) TouchUser(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, "UPDATE users SET last_seen_at = ? WHERE id = ?", s.unix(), id)
	return err
}
