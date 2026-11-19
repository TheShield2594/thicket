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
	"sync"
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
	// scrub removes secret values from text before it's written to the
	// trail, conversations, the inbox and run history (set by the app).
	scrub func(string) string
}

// SetScrubber sets what removes secrets from stored text.
func (s *Store) SetScrubber(f func(string) string) { s.scrub = f }

func (s *Store) clean(text string) string {
	if s.scrub == nil || text == "" {
		return text
	}
	return s.scrub(text)
}

// Open opens (creating if needed) the database at path and applies any
// pending migrations. If the database already holds data and migrations are
// pending, a snapshot is taken first so an upgrade can always be undone.
func Open(ctx context.Context, path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return openDB(ctx, path, 0)
}

// OpenExisting opens a database that is already there without applying
// migrations, so `thicket update` can read what it is about to change
// (its schema version, the version that last ran it) and back it up
// first. Most of the store's methods assume the current schema: use only
// SchemaVersion, Setting and SnapshotTo on the result.
func OpenExisting(ctx context.Context, path string) (*Store, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	return openDB(ctx, path, -1)
}

// OpenAtSchema opens the database migrated only as far as version, the
// way an older Thicket would have left it. It exists for tests of
// upgrades.
func OpenAtSchema(ctx context.Context, path string, version int) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return openDB(ctx, path, version)
}

// openDB opens path and migrates up to limit (-1: not at all).
func openDB(ctx context.Context, path string, limit int) (*Store, error) {
	dsn := "file:" + path + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// One connection: Thicket's write volume is tiny and this rules out
	// SQLITE_BUSY between our own goroutines.
	db.SetMaxOpenConns(1)
	s := &Store{db: db, path: path, now: time.Now}
	if limit >= 0 {
		err = s.migrateTo(ctx, limit)
	} else {
		err = s.Ping(ctx)
	}
	if err != nil {
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
	injectedMu.Lock()
	for i, q := range injected {
		v := len(out) + 1
		out = append(out, migration{v, fmt.Sprintf("%04d_injected_%d.sql", v, i+1), q})
	}
	injectedMu.Unlock()
	for i, m := range out {
		if m.version != i+1 {
			return nil, fmt.Errorf("migrations must be numbered 1..n without gaps; found %s at position %d", m.name, i+1)
		}
	}
	return out, nil
}

var (
	injectedMu sync.Mutex
	injected   []string
)

// InjectMigration adds a migration after the ones this binary carries,
// until the returned function is called. It exists for tests of what
// happens when a migration goes wrong (doctor's memory check, `thicket
// update` rolling back); nothing else calls it.
func InjectMigration(sqlText string) (remove func()) {
	injectedMu.Lock()
	defer injectedMu.Unlock()
	injected = append(injected, sqlText)
	return func() {
		injectedMu.Lock()
		defer injectedMu.Unlock()
		injected = injected[:0]
	}
}

// SchemaVersion returns the current applied schema version.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var v sql.NullInt64
	err := s.db.QueryRowContext(ctx, "SELECT MAX(version) FROM schema_migrations").Scan(&v)
	return int(v.Int64), err
}

func (s *Store) migrate(ctx context.Context) error { return s.migrateTo(ctx, 0) }

// migrateTo applies pending migrations up to version limit (0: all).
func (s *Store) migrateTo(ctx context.Context, limit int) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at INTEGER NOT NULL)`); err != nil {
		return err
	}
	migs, err := loadMigrations()
	if err != nil {
		return err
	}
	if limit > 0 && limit < len(migs) {
		migs = migs[:limit]
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

// QuickCheck runs SQLite's integrity check (the fast variant).
func (s *Store) QuickCheck(ctx context.Context) error {
	var res string
	if err := s.db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&res); err != nil {
		return err
	}
	if res != "ok" {
		return errors.New(res)
	}
	return nil
}

// AppliedMigration is one row of the migration history.
type AppliedMigration struct {
	Version   int
	Name      string
	AppliedAt time.Time
}

// AppliedMigrations lists migrations applied to this database, oldest first.
func (s *Store) AppliedMigrations(ctx context.Context) ([]AppliedMigration, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT version, name, applied_at FROM schema_migrations ORDER BY version")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AppliedMigration
	for rows.Next() {
		var m AppliedMigration
		var at int64
		if err := rows.Scan(&m.Version, &m.Name, &at); err != nil {
			return nil, err
		}
		m.AppliedAt = time.Unix(at, 0)
		out = append(out, m)
	}
	return out, rows.Err()
}

// KnownMigrations is how many migrations this binary carries.
func KnownMigrations() (int, error) {
	m, err := loadMigrations()
	return len(m), err
}
