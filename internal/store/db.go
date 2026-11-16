// Package store owns Thicket's SQLite database: versioned migrations and
// all data access. Nothing else in Thicket writes SQL.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// ErrNotFound is returned when a single-row lookup finds nothing.
var ErrNotFound = errors.New("not found")

type Store struct {
	db   *sql.DB
	path string
	now  func() time.Time
}

// Open opens (creating if needed) the database at path and applies any
// pending migrations. If the database already holds data and migrations are
// pending, a snapshot is taken first so an upgrade can always be undone.
func Open(ctx context.Context, path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	dsn := "file:" + path + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection: Thicket's write volume is tiny and this rules out
	// SQLITE_BUSY between our own goroutines.
	db.SetMaxOpenConns(1)
	s := &Store{db: db, path: path, now: time.Now}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }
func (s *Store) Path() string { return s.path }
func (s *Store) DB() *sql.DB  { return s.db }

// SetClock overrides the clock (tests).
func (s *Store) SetClock(now func() time.Time) { s.now = now }

func (s *Store) unix() int64 { return s.now().Unix() }

func (s *Store) Ping(ctx context.Context) error {
	var one int
	return s.db.QueryRowContext(ctx, "SELECT 1").Scan(&one)
}

type migration struct {
	version int
	name    string
	sql     string
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, err
	}
	var out []migration
	for _, e := range entries {
		name := e.Name()
		num, _, ok := strings.Cut(name, "_")
		if !ok {
			return nil, fmt.Errorf("migration %s: name must be NNNN_description.sql", name)
		}
		v, err := strconv.Atoi(num)
		if err != nil {
			return nil, fmt.Errorf("migration %s: %w", name, err)
		}
		b, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return nil, err
		}
		out = append(out, migration{v, name, string(b)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	for i, m := range out {
		if m.version != i+1 {
			return nil, fmt.Errorf("migrations must be numbered 1..n without gaps; found %s at position %d", m.name, i+1)
		}
	}
	return out, nil
}

// SchemaVersion returns the current applied schema version.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v sql.NullInt64
	err := s.db.QueryRowContext(ctx, "SELECT MAX(version) FROM schema_migrations").Scan(&v)
	return int(v.Int64), err
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at INTEGER NOT NULL)`); err != nil {
		return err
	}
	migs, err := loadMigrations()
	if err != nil {
		return err
	}
	cur, err := s.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	if cur > len(migs) {
		return fmt.Errorf("database schema is version %d but this Thicket only knows up to %d; refusing to run an older binary against a newer database (restore a backup or upgrade Thicket)", cur, len(migs))
	}
	if cur == len(migs) {
		return nil
	}
	if cur > 0 {
		snap := s.path + ".pre-v" + strconv.Itoa(len(migs)) + "." + s.now().UTC().Format("20060102T150405") + ".bak"
		if err := s.SnapshotTo(ctx, snap); err != nil {
			return fmt.Errorf("pre-migration backup failed, not migrating: %w", err)
		}
	}
	for _, m := range migs[cur:] {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, m.sql); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %s: %w", m.name, err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)", m.version, m.name, s.unix()); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// SnapshotTo writes a consistent copy of the database to dest.
func (s *Store) SnapshotTo(ctx context.Context, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return err
	}
	os.Remove(dest)
	_, err := s.db.ExecContext(ctx, "VACUUM INTO ?", dest)
	return err
}

// --- small helpers ---

func nullInt(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func timeOf(v sql.NullInt64) time.Time {
	if !v.Valid || v.Int64 == 0 {
		return time.Time{}
	}
	return time.Unix(v.Int64, 0)
}

func unixOrNil(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.Unix()
}
