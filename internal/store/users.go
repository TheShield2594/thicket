package store

import (
	"context"
	"database/sql"
	"errors"
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
}

func (u *User) IsAdmin() bool { return u.Role == "owner" || u.Role == "admin" }

// Location returns the user's timezone, falling back to UTC.
func (u *User) Location() *time.Location {
	if u.Timezone != "" {
		if loc, err := time.LoadLocation(u.Timezone); err == nil {
			return loc
		}
	}
	return time.UTC
}

const userCols = "id, channel, external_id, name, role, timezone, onboarded_at, last_seen_at, created_at"

func scanUser(r interface{ Scan(...any) error }) (*User, error) {
	var u User
	var onb, seen sql.NullInt64
	var created int64
	if err := r.Scan(&u.ID, &u.Channel, &u.ExternalID, &u.Name, &u.Role, &u.Timezone, &onb, &seen, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	u.OnboardedAt, u.LastSeenAt, u.CreatedAt = timeOf(onb), timeOf(seen), time.Unix(created, 0)
	return &u, nil
}

func (s *Store) UserByExternal(ctx context.Context, channel, ext string) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, "SELECT "+userCols+" FROM users WHERE channel = ? AND external_id = ?", channel, ext))
}

func (s *Store) UserByID(ctx context.Context, id int64) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, "SELECT "+userCols+" FROM users WHERE id = ?", id))
}

func (s *Store) CreateUser(ctx context.Context, channel, ext, name, role string) (*User, error) {
	res, err := s.db.ExecContext(ctx, "INSERT INTO users (channel, external_id, name, role, created_at) VALUES (?, ?, ?, ?, ?)",
		channel, ext, name, role, s.unix())
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.UserByID(ctx, id)
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
	_, err := s.db.ExecContext(ctx, "UPDATE users SET role = ? WHERE id = ?", role, id)
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
