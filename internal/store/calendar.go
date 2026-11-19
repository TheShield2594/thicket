package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// SprigEvent is something a sprig put on the calendar (a "sprig output"):
// a dinner from a meal planner, a release date. It links back to its source.
type SprigEvent struct {
	ID          int64     `json:"id"`
	UserID      int64     `json:"user_id"` // whose calendar (who asked)
	Character   string    `json:"sprig"`
	Kind        string    `json:"kind"`
	Title       string    `json:"title"`
	StartsAt    time.Time `json:"starts_at"`
	EndsAt      time.Time `json:"ends_at,omitempty"`
	AllDay      bool      `json:"all_day,omitempty"`
	Details     string    `json:"details,omitempty"`
	SourceLabel string    `json:"source_label,omitempty"`
	SourceURL   string    `json:"source_url,omitempty"`
	// Shared events show on every adult's calendar.
	Shared    bool      `json:"shared,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

const sprigEventCols = "id, user_id, character, kind, title, starts_at, ends_at, all_day, details, source_label, source_url, shared, created_at"

func scanSprigEvent(r interface{ Scan(...any) error }) (*SprigEvent, error) {
	var e SprigEvent
	var start, created int64
	var end sql.NullInt64
	var allDay, shared int
	if err := r.Scan(&e.ID, &e.UserID, &e.Character, &e.Kind, &e.Title, &start, &end, &allDay, &e.Details, &e.SourceLabel, &e.SourceURL, &shared, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	e.StartsAt, e.EndsAt, e.CreatedAt = time.Unix(start, 0), timeOf(end), time.Unix(created, 0)
	e.AllDay, e.Shared = allDay == 1, shared == 1
	return &e, nil
}

func (s *Store) AddSprigEvent(ctx context.Context, e SprigEvent) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO sprig_events (user_id, character, kind, title, starts_at, ends_at, all_day, details, source_label, source_url, shared, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.UserID, e.Character, e.Kind, s.clean(e.Title), e.StartsAt.Unix(), unixOrNil(e.EndsAt), boolInt(e.AllDay), s.clean(e.Details),
		s.clean(e.SourceLabel), e.SourceURL, boolInt(e.Shared), s.unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// SprigEventByID returns a live event.
func (s *Store) SprigEventByID(ctx context.Context, id int64) (*SprigEvent, error) {
	return scanSprigEvent(s.db.QueryRowContext(ctx, "SELECT "+sprigEventCols+" FROM sprig_events WHERE id = ? AND deleted_at IS NULL", id))
}

// DeleteSprigEvent takes an event off the calendar.
func (s *Store) DeleteSprigEvent(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, "UPDATE sprig_events SET deleted_at = ? WHERE id = ? AND deleted_at IS NULL", s.unix(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SprigEventAny returns an event, removed or not.
func (s *Store) SprigEventAny(ctx context.Context, id int64) (*SprigEvent, error) {
	return scanSprigEvent(s.db.QueryRowContext(ctx, "SELECT "+sprigEventCols+" FROM sprig_events WHERE id = ?", id))
}

// RestoreSprigEvent puts a removed event back.
func (s *Store) RestoreSprigEvent(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, "UPDATE sprig_events SET deleted_at = NULL WHERE id = ?", id)
	return err
}

// SprigEventsFor returns the events a person sees between from and to:
// their own, plus shared ones.
func (s *Store) SprigEventsFor(ctx context.Context, userID int64, from, to time.Time) ([]SprigEvent, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+sprigEventCols+` FROM sprig_events WHERE deleted_at IS NULL
		AND (user_id = ? OR shared = 1) AND starts_at < ? AND COALESCE(ends_at, starts_at) >= ? ORDER BY starts_at, id`,
		userID, to.Unix(), from.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SprigEvent
	for rows.Next() {
		e, err := scanSprigEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

// CalendarSource is a calendar an adult connected, read-only.
type CalendarSource struct {
	ID        int64     `json:"id"`
	UserID    int64     `json:"user_id"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind"` // ics | caldav
	URL       string    `json:"-"`    // may carry a private token (ICS links do)
	Username  string    `json:"-"`
	Secret    string    `json:"-"` // name of the password in the encrypted store
	Shared    bool      `json:"shared"`
	Enabled   bool      `json:"enabled"`
	LastError string    `json:"last_error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

const calSourceCols = "id, user_id, name, kind, url, username, secret, shared, enabled, last_error, created_at"

func (s *Store) AddCalendarSource(ctx context.Context, c CalendarSource) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO calendar_sources (user_id, name, kind, url, username, secret, shared, enabled, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 1, ?)`, c.UserID, s.clean(c.Name), c.Kind, c.URL, c.Username, c.Secret, boolInt(c.Shared), s.unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// CalendarSourcesFor returns the calendars a person sees: their own and
// shared ones (0 = every calendar).
func (s *Store) CalendarSourcesFor(ctx context.Context, userID int64) ([]CalendarSource, error) {
	q, args := "SELECT "+calSourceCols+" FROM calendar_sources", []any{}
	if userID != 0 {
		q += " WHERE user_id = ? OR shared = 1"
		args = append(args, userID)
	}
	rows, err := s.db.QueryContext(ctx, q+" ORDER BY id", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CalendarSource
	for rows.Next() {
		var c CalendarSource
		var shared, enabled int
		var created int64
		if err := rows.Scan(&c.ID, &c.UserID, &c.Name, &c.Kind, &c.URL, &c.Username, &c.Secret, &shared, &enabled, &c.LastError, &created); err != nil {
			return nil, err
		}
		c.Shared, c.Enabled, c.CreatedAt = shared == 1, enabled == 1, time.Unix(created, 0)
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) SetCalendarSourceError(ctx context.Context, id int64, msg string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE calendar_sources SET last_error = ? WHERE id = ?", s.clean(msg), id)
	return err
}

// DeleteCalendarSource removes one of a person's calendars.
func (s *Store) DeleteCalendarSource(ctx context.Context, userID, id int64) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM calendar_sources WHERE id = ? AND user_id = ?", id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
