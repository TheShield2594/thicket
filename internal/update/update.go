// Package update moves an install's data to the version of the running
// binary: it shows what changed, takes a full backup, runs migrations and
// checks the result, and puts everything back exactly as it was if the
// check fails. It is run from the new version (the new image, or the new
// binary) while the old one is stopped; it never replaces a binary.
package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/theshield2594/thicket/internal/backup"
	"github.com/theshield2594/thicket/internal/changelog"
	"github.com/theshield2594/thicket/internal/config"
	"github.com/theshield2594/thicket/internal/doctor"
	"github.com/theshield2594/thicket/internal/store"
)

// dataChecks are the doctor findings an update is judged by: the ones
// about the data it just changed. A model server or chat app that's
// unreachable is reported but doesn't undo an update.
// A migration that loses memory fails "memory", so it rolls back too.
var dataChecks = map[string]bool{"config": true, "database": true, "secrets": true, "memory": true}

type Updater struct {
	Cfg *config.Config
	// Version is this binary's; Changelog its CHANGELOG.md.
	Version   string
	Changelog string
	Out       io.Writer
	// Confirm asks before anything is changed; nil means yes.
	Confirm func(question string) bool
	// Force allows moving to an older version with the same schema.
	Force bool
	// Doctor runs the checks after migrating; nil runs `thicket doctor`'s.
	Doctor func(ctx context.Context) []doctor.Check
	// Running reports whether a server is using the data; nil looks for
	// its heartbeat and asks its /health.
	Running func() bool
}

// Result is what happened, as recorded in update.log.
type Result struct {
	At         time.Time `json:"at"`
	From       string    `json:"from"`
	To         string    `json:"to"`
	FromSchema int       `json:"from_schema"`
	ToSchema   int       `json:"to_schema"`
	Outcome    string    `json:"outcome"` // updated | rolled back | nothing to do
	Detail     string    `json:"detail,omitempty"`
	Backup     string    `json:"backup,omitempty"`
}

// ErrDeclined is returned when the person answers no.
var ErrDeclined = errors.New("update cancelled; nothing was changed")

func (u *Updater) say(format string, args ...any) { fmt.Fprintf(u.Out, format+"\n", args...) }

func (u *Updater) dbPath() string { return filepath.Join(u.Cfg.DataDir, "thicket.db") }

func (u *Updater) running() bool {
	if u.Running != nil {
		return u.Running()
	}
	if backup.ServerRunning(u.Cfg.DataDir) {
		return true
	}
	addr := u.Cfg.HTTPAddr
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Get("http://" + addr + "/health")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

// Run performs the update. On a failed migration or check it restores
// the data and returns an error describing what failed; the Result says
// what was done either way.
func (u *Updater) Run(ctx context.Context) (Result, error) {
	res := Result{At: time.Now().UTC(), To: u.Version}
	known, err := store.KnownMigrations()
	if err != nil {
		return res, err
	}
	res.ToSchema = known

	// What's there now, read without changing it.
	old, err := store.OpenExisting(ctx, u.dbPath())
	if errors.Is(err, os.ErrNotExist) {
		return res, fmt.Errorf("there's no Thicket data in %s yet: nothing to update (run `thicket init` to set one up)", u.Cfg.DataDir)
	}
	if err != nil {
		return res, fmt.Errorf("the database didn't open: %w", err)
	}
	res.FromSchema, err = old.SchemaVersion(ctx)
	if err == nil {
		res.From, _, err = old.Setting(ctx, store.VersionKey)
	}
	old.Close()
	if err != nil {
		return res, fmt.Errorf("the database couldn't be read: %w", err)
	}
	from := res.From
	if from == "" {
		from = "an earlier version"
	}

	switch {
	case res.FromSchema > known:
		return res, fmt.Errorf("this data was last used by a newer Thicket (schema v%d; this is %s, which knows up to v%d). Going back across a schema change isn't possible in place: start the newer version again, or restore the backup that was taken before that update (in %s) with this version", res.FromSchema, u.Version, known, filepath.Join(u.Cfg.DataDir, "backups"))
	case res.FromSchema == known && res.From == u.Version:
		res.Outcome = "nothing to do"
		u.say("Already on %s (schema v%d): nothing to do.", u.Version, known)
		return res, nil
	case changelog.IsRelease(res.From) && changelog.IsRelease(u.Version) && changelog.Compare(u.Version, res.From) < 0 && !u.Force:
		return res, fmt.Errorf("%s is older than %s, which this data last ran on. The schema is the same, so it can work: pass --force if you mean it", u.Version, res.From)
	}
	if u.running() {
		return res, errors.New("Thicket is running on this data. Stop it first (docker compose stop thicket), then run the update from the new version")
	}

	// 1. What's changing.
	u.say("Updating %s (schema v%d) to %s (schema v%d).", from, res.FromSchema, u.Version, known)
	notes := changelog.Between(u.Changelog, res.From, u.Version)
	if !changelog.IsRelease(u.Version) {
		notes = changelog.Between(u.Changelog, res.From, "")
	}
	if len(notes) == 0 {
		u.say("\nThe changelog lists nothing between them.")
	}
	for _, s := range notes {
		u.say("\n## %s\n\n%s", s.Heading, s.Body)
	}
	u.say("")
	if u.Confirm != nil && !u.Confirm("Back up, then update?") {
		return res, ErrDeclined
	}

	// 2. Keep the data exactly as it is, then a full backup in the usual
	// format (the one `thicket restore` reads).
	stamp := res.At.Format("20060102-150405")
	backups := filepath.Join(u.Cfg.DataDir, "backups")
	raw, err := os.MkdirTemp(backups, ".pre-update-"+stamp+"-")
	if errors.Is(err, os.ErrNotExist) {
		if err = os.MkdirAll(backups, 0o700); err == nil {
			raw, err = os.MkdirTemp(backups, ".pre-update-"+stamp+"-")
		}
	}
	if err != nil {
		return res, fmt.Errorf("couldn't make room for a copy of the current data, so nothing was changed: %w", err)
	}
	saved, err := saveRaw(u.dbPath(), u.Cfg.CharactersDir, raw)
	if err != nil {
		os.RemoveAll(raw)
		return res, fmt.Errorf("couldn't set the current data aside, so nothing was changed: %w", err)
	}
	fail := func(what string) (Result, error) {
		res.Outcome, res.Detail = "rolled back", what
		if err := saved.restore(); err != nil {
			res.Outcome = "rollback failed"
			u.log(res)
			return res, fmt.Errorf("%s, and putting the data back failed too (%v). The untouched copy is in %s and the backup is %s", what, err, raw, res.Backup)
		}
		os.RemoveAll(raw)
		u.log(res)
		u.say("\nThe update did not go through: %s.", what)
		u.say("Everything is back exactly as it was. Keep running %s (set THICKET_VERSION back if you changed it) and report this.", from)
		return res, fmt.Errorf("update to %s rolled back: %s", u.Version, what)
	}

	res.Backup = filepath.Join(backups, "pre-update-"+stamp+".tar.gz")
	for n := 2; ; n++ { // two updates in one second still get a backup each
		if _, err := os.Stat(res.Backup); err != nil {
			break
		}
		res.Backup = filepath.Join(backups, fmt.Sprintf("pre-update-%s-%d.tar.gz", stamp, n))
	}
	if err := u.backup(ctx, res.Backup); err != nil {
		res.Backup = ""
		return fail("the backup before updating failed (" + err.Error() + ")")
	}
	u.say("Backed up to %s", res.Backup)

	// 3. Migrate.
	db, err := store.Open(ctx, u.dbPath())
	if err != nil {
		return fail("the migrations failed (" + err.Error() + ")")
	}
	db.Close()
	u.say("Migrated: schema v%d to v%d.", res.FromSchema, known)

	// 4. Check.
	run := u.Doctor
	if run == nil {
		run = (&doctor.Doctor{Cfg: u.Cfg}).Run
	}
	var bad []string
	for _, c := range run(ctx) {
		mark := "ok  "
		switch {
		case c.OK:
		case dataChecks[c.Name]:
			mark = "FAIL"
			bad = append(bad, c.Name+": "+c.Detail)
		default:
			mark = "warn"
		}
		u.say("[%s] %-16s %s", mark, c.Name, c.Detail)
	}
	if len(bad) > 0 {
		return fail("the check after migrating failed (" + strings.Join(bad, "; ") + ")")
	}

	// Done: remember the version, and say so in the trail.
	res.Outcome = "updated"
	db, err = store.Open(ctx, u.dbPath())
	if err != nil {
		return fail("the database didn't reopen after the check (" + err.Error() + ")")
	}
	defer db.Close()
	if err := db.SetSetting(ctx, store.VersionKey, u.Version); err != nil {
		return res, err
	}
	if err := db.AddTrail(ctx, store.TrailEntry{Kind: store.TrailUpdate,
		Summary: fmt.Sprintf("Thicket updated from %s to %s", from, u.Version),
		Detail:  fmt.Sprintf("schema v%d to v%d; backup %s", res.FromSchema, known, filepath.Base(res.Backup))}); err != nil {
		return res, err
	}
	os.RemoveAll(raw)
	u.log(res)
	u.say("\nUpdated to %s. Start it: docker compose up -d (or thicket serve).", u.Version)
	return res, nil
}

// backup writes the pre-update archive from the database as it is, before
// any migration.
func (u *Updater) backup(ctx context.Context, path string) error {
	db, err := store.OpenExisting(ctx, u.dbPath())
	if err != nil {
		return err
	}
	defer db.Close()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	err = backup.Export(ctx, db, u.Cfg.DataDir, u.Cfg.CharactersDir, false, f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
	}
	return err
}

// log appends the result to update.log in the data directory: the one
// record that survives a rollback (the trail lives in the database that
// was just put back).
func (u *Updater) log(res Result) {
	line, _ := json.Marshal(res)
	f, err := os.OpenFile(filepath.Join(u.Cfg.DataDir, "update.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(line, '\n'))
}

// History reads update.log, oldest first.
func History(dataDir string) ([]Result, error) {
	b, err := os.ReadFile(filepath.Join(dataDir, "update.log"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Result
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var r Result
		if json.Unmarshal([]byte(line), &r) == nil {
			out = append(out, r)
		}
	}
	return out, nil
}

// --- the untouched copy ---

// rawCopy is the database files and sprig folders, byte for byte, as
// they were before the update.
type rawCopy struct {
	dir      string
	dbPath   string
	charsDir string
	hadChars bool
	started  time.Time
}

var dbFiles = []string{"", "-wal", "-shm"}

func saveRaw(dbPath, charsDir, dir string) (*rawCopy, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	r := &rawCopy{dir: dir, dbPath: dbPath, charsDir: charsDir, started: time.Now()}
	for _, ext := range dbFiles {
		if err := copyFile(dbPath+ext, filepath.Join(dir, "thicket.db"+ext)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	if _, err := os.Stat(charsDir); err == nil {
		r.hadChars = true
		if err := copyTree(charsDir, filepath.Join(dir, "characters")); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// restore puts the copy back: the database files and sprig folders end
// up byte-identical to before, and what the failed update left beside
// them (a pre-migration snapshot) is removed.
func (r *rawCopy) restore() error {
	for _, ext := range dbFiles {
		if err := os.Remove(r.dbPath + ext); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := copyFile(filepath.Join(r.dir, "thicket.db"+ext), r.dbPath+ext); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	snaps, _ := filepath.Glob(r.dbPath + ".pre-v*.bak")
	for _, s := range snaps {
		if info, err := os.Stat(s); err == nil && !info.ModTime().Before(r.started.Add(-time.Second)) {
			os.Remove(s)
		}
	}
	if err := os.RemoveAll(r.charsDir); err != nil {
		return err
	}
	if r.hadChars {
		return copyTree(filepath.Join(r.dir, "characters"), r.charsDir)
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chtimes(dst, info.ModTime(), info.ModTime())
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			info, err := d.Info()
			if err != nil {
				return err
			}
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		case d.Type().IsRegular():
			return copyFile(path, target)
		}
		return nil // links and devices aren't part of a sprig
	})
}
