// Package backup exports and restores a Thicket install (database plus
// character files). Importing single characters lives in charimport.
package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/theshield2594/thicket/internal/store"
)

type Manifest struct {
	// Format 2 archives carry every file in a sprig's folder (avatar
	// settings as well as voice tests); format 1 had voice tests only.
	Format        int       `json:"format"`
	SchemaVersion int       `json:"schema_version"`
	CreatedAt     time.Time `json:"created_at"`
	IncludesKey   bool      `json:"includes_key"`
}

// Format is the archive format this build writes; Restore reads this and
// anything older.
const Format = 2

// Export writes a .tar.gz with the database (a consistent copy, safe to
// take while Thicket runs), every character file and every sprig folder.
// The secret key is only included if asked: without it the encrypted
// secrets in the database can't be read, which is usually what you want
// for an off-site copy.
func Export(ctx context.Context, db *store.Store, dataDir, charsDir string, includeKey bool, out io.Writer) error {
	tmp, err := os.MkdirTemp("", "thicket-export-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	dbCopy := filepath.Join(tmp, "thicket.db")
	if err := db.SnapshotTo(ctx, dbCopy); err != nil {
		return fmt.Errorf("snapshot database: %w", err)
	}
	ver, err := db.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	gz := gzip.NewWriter(out)
	tw := tar.NewWriter(gz)
	man, _ := json.MarshalIndent(Manifest{Format: Format, SchemaVersion: ver, CreatedAt: time.Now().UTC(), IncludesKey: includeKey}, "", "  ")
	if err := addBytes(tw, "manifest.json", man, 0o644); err != nil {
		return err
	}
	if err := addFile(tw, dbCopy, "thicket.db", 0o600); err != nil {
		return err
	}
	entries, err := os.ReadDir(charsDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") || strings.HasSuffix(e.Name(), ".tmp") {
			continue
		}
		if e.IsDir() {
			// A sprig's folder travels whole: voice tests, avatar settings
			// and whatever else it keeps there.
			root := filepath.Join(charsDir, e.Name())
			err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if !d.Type().IsRegular() || strings.HasSuffix(d.Name(), ".tmp") {
					return nil
				}
				rel, err := filepath.Rel(charsDir, path)
				if err != nil {
					return err
				}
				return addFile(tw, path, "characters/"+filepath.ToSlash(rel), 0o644)
			})
			if err != nil {
				return err
			}
			continue
		}
		if err := addFile(tw, filepath.Join(charsDir, e.Name()), "characters/"+e.Name(), 0o644); err != nil {
			return err
		}
	}
	if includeKey {
		if _, err := os.Stat(filepath.Join(dataDir, "secret.key")); err == nil {
			if err := addFile(tw, filepath.Join(dataDir, "secret.key"), "secret.key", 0o600); err != nil {
				return err
			}
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

func addBytes(tw *tar.Writer, name string, b []byte, mode int64) error {
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(b)), ModTime: time.Now()}); err != nil {
		return err
	}
	_, err := tw.Write(b)
	return err
}

func addFile(tw *tar.Writer, path, name string, mode int64) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: info.Size(), ModTime: info.ModTime()}); err != nil {
		return err
	}
	_, err = io.Copy(tw, f)
	return err
}

// sprigFilePath matches a file in a sprig's folder in an archive, up to
// six folders deep. Names may be anything Export includes (hidden ones
// too) except separators and control characters; ".." is refused
// separately.
var sprigFilePath = regexp.MustCompile(`^characters/[a-z0-9][a-z0-9_-]*(/[^/\\\x00-\x1f]+){1,6}$`)

// maxEntry bounds any single file in an archive.
const maxEntry int64 = 64 << 30

// Restore unpacks an export into dataDir. The archive is extracted into a
// staging directory and checked first; only then are the installation's
// files swapped in. Existing files are moved aside (never deleted), and if
// anything fails part-way the swap is undone so the database and key stay
// consistent. An existing database is only replaced with force.
func Restore(archive io.Reader, dataDir, charsDir string, force bool) error {
	dbPath := filepath.Join(dataDir, "thicket.db")
	if !force && ServerRunning(dataDir) {
		return fmt.Errorf("Thicket is running on %s: stop it first (docker compose stop thicket), then restore with --force to replace its data", dataDir)
	}
	if _, err := os.Stat(dbPath); err == nil && !force {
		return fmt.Errorf("%s already exists; stop Thicket and pass --force to replace it (the current files are kept as .before-restore)", dbPath)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(dataDir, ".restore-staging-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)

	// 1. Extract everything into staging.
	type item struct{ staged, dest string }
	var items []item
	var man *Manifest
	gz, err := gzip.NewReader(archive)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(h.Name)
		if strings.Contains(name, "..") || filepath.IsAbs(name) {
			return fmt.Errorf("refusing unsafe path %q in archive", h.Name)
		}
		var dest string
		switch {
		case name == "thicket.db":
			dest = dbPath
		case name == "secret.key":
			dest = filepath.Join(dataDir, "secret.key")
		case name == "manifest.json" && h.Typeflag == tar.TypeReg && h.Size <= 1<<20:
			b, err := io.ReadAll(io.LimitReader(tr, h.Size))
			if err != nil {
				return fmt.Errorf("manifest.json in the archive is unreadable: %w", err)
			}
			man = &Manifest{}
			if err := json.Unmarshal(b, man); err != nil {
				return fmt.Errorf("manifest.json in the archive is not valid: %w", err)
			}
			continue
		case sprigFilePath.MatchString(filepath.ToSlash(name)) && h.Typeflag == tar.TypeReg:
			parts := strings.Split(filepath.ToSlash(name), "/")
			dest = filepath.Join(append([]string{charsDir}, parts[1:]...)...)
		case strings.HasPrefix(name, "characters/") && strings.Count(filepath.ToSlash(name), "/") == 1 && h.Typeflag == tar.TypeReg:
			dest = filepath.Join(charsDir, filepath.Base(name))
		default:
			continue
		}
		if h.Size < 0 || h.Size > maxEntry {
			return fmt.Errorf("%s in the archive is too large (%d bytes)", name, h.Size)
		}
		// Numbered, not derived from the name: two archive paths can never
		// share a staging file.
		staged := filepath.Join(stage, fmt.Sprintf("entry-%04d", len(items)))
		f, err := os.OpenFile(staged, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		n, err := io.CopyN(f, tr, h.Size)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil || n != h.Size {
			return fmt.Errorf("%s in the archive is truncated or unreadable: %v", name, err)
		}
		items = append(items, item{staged, dest})
	}

	// 2. Validate before touching the installation.
	var hasDB bool
	for _, it := range items {
		if it.dest == dbPath {
			hdr := make([]byte, 16)
			f, err := os.Open(it.staged)
			if err != nil {
				return err
			}
			_, err = io.ReadFull(f, hdr)
			f.Close()
			if err != nil || string(hdr) != "SQLite format 3\x00" {
				return errors.New("thicket.db in the archive is not a SQLite database")
			}
			hasDB = true
		}
	}
	if !hasDB {
		return errors.New("archive has no thicket.db; is this a Thicket backup?")
	}
	if man == nil {
		return errors.New("archive has no manifest.json; is this a Thicket backup?")
	}
	if man.Format > Format {
		return fmt.Errorf("this backup is format %d and this Thicket reads up to %d: restore it with the Thicket version that made it, or a newer one", man.Format, Format)
	}
	if known, err := store.KnownMigrations(); err != nil {
		return err
	} else if man.SchemaVersion > known {
		return fmt.Errorf("this backup's database is schema v%d and this Thicket only knows up to v%d: restore it with a newer Thicket", man.SchemaVersion, known)
	}

	// 3. Swap files in, remembering how to undo each step.
	suffix := ".before-restore-" + time.Now().UTC().Format("20060102T150405")
	var undo []func()
	rollback := func() {
		for k := len(undo) - 1; k >= 0; k-- {
			undo[k]()
		}
	}
	moveAside := func(path string) error {
		if _, err := os.Stat(path); err != nil {
			return nil
		}
		if err := os.Rename(path, path+suffix); err != nil {
			return err
		}
		undo = append(undo, func() { os.Rename(path+suffix, path) })
		return nil
	}
	for _, ext := range []string{"-wal", "-shm"} {
		if err := moveAside(dbPath + ext); err != nil {
			rollback()
			return err
		}
	}
	for _, it := range items {
		if err := os.MkdirAll(filepath.Dir(it.dest), 0o700); err != nil {
			rollback()
			return err
		}
		if err := moveAside(it.dest); err != nil {
			rollback()
			return err
		}
		if err := os.Rename(it.staged, it.dest); err != nil {
			rollback()
			return fmt.Errorf("restore %s: %w (nothing was changed)", filepath.Base(it.dest), err)
		}
		dest := it.dest
		undo = append(undo, func() { os.Remove(dest) })
		mode := os.FileMode(0o600)
		if strings.HasPrefix(dest, charsDir) {
			mode = 0o644
		}
		_ = os.Chmod(dest, mode)
	}
	return nil
}

// Snapshot writes a dated database copy into dir and keeps the newest keep.
func Snapshot(ctx context.Context, db *store.Store, dir string, keep int) (string, error) {
	name := "thicket-" + time.Now().UTC().Format("20060102") + ".db"
	path := filepath.Join(dir, name)
	if err := db.SnapshotTo(ctx, path); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return path, err
	}
	var snaps []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "thicket-") && strings.HasSuffix(e.Name(), ".db") {
			snaps = append(snaps, e.Name())
		}
	}
	sort.Strings(snaps)
	for len(snaps) > keep {
		os.Remove(filepath.Join(dir, snaps[0]))
		snaps = snaps[1:]
	}
	return path, nil
}

// --- is the server running? ---

// The server touches this file while it runs, so a command in another
// process (or another container on the same volume) can tell.
const aliveFile = "server.alive"

// aliveFor is how long a heartbeat counts: a few missed beats, so a
// crashed server stops blocking a restore within minutes.
const aliveFor = 2 * time.Minute

// Heartbeat marks the server as running on dataDir. Call it at start and
// about every 30 seconds.
func Heartbeat(dataDir string) error {
	return os.WriteFile(filepath.Join(dataDir, aliveFile), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o600)
}

// ClearHeartbeat marks the server as stopped.
func ClearHeartbeat(dataDir string) { os.Remove(filepath.Join(dataDir, aliveFile)) }

// ServerRunning reports whether a server marked dataDir recently.
func ServerRunning(dataDir string) bool {
	info, err := os.Stat(filepath.Join(dataDir, aliveFile))
	return err == nil && time.Since(info.ModTime()) < aliveFor
}

// --- scheduled backups ---

// Info describes a backup archive on disk.
type Info struct {
	Path string
	Size int64
	At   time.Time
}

const (
	scheduledPrefix = "thicket-backup-"
	scheduledSuffix = ".tar.gz"
)

// Scheduled writes a full backup into dir (never the key: the folder may
// be a share or a sync target) and keeps the newest keep. The archive is
// written under a temporary name and renamed, so a half-written file is
// never mistaken for a backup.
func Scheduled(ctx context.Context, db *store.Store, dataDir, charsDir, dir string, keep int) (Info, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Info{}, err
	}
	final := filepath.Join(dir, scheduledPrefix+time.Now().UTC().Format("20060102-150405")+scheduledSuffix)
	f, err := os.CreateTemp(dir, ".backup-*.tmp")
	if err != nil {
		return Info{}, err
	}
	tmp := f.Name()
	err = Export(ctx, db, dataDir, charsDir, false, f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, final)
	}
	if err != nil {
		os.Remove(tmp)
		return Info{}, err
	}
	if keep < 1 {
		keep = 1
	}
	all := list(dir)
	for len(all) > keep {
		os.Remove(all[0].Path)
		all = all[1:]
	}
	info, err := os.Stat(final)
	if err != nil {
		return Info{}, err
	}
	return Info{Path: final, Size: info.Size(), At: info.ModTime()}, nil
}

// Latest returns the newest scheduled backup in dir.
func Latest(dir string) (Info, bool) {
	all := list(dir)
	if len(all) == 0 {
		return Info{}, false
	}
	return all[len(all)-1], true
}

// LatestSnapshot returns the newest daily database snapshot in dir.
func LatestSnapshot(dir string) (Info, bool) {
	var best Info
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "thicket-") || !strings.HasSuffix(e.Name(), ".db") {
			continue
		}
		if info, err := e.Info(); err == nil && info.ModTime().After(best.At) {
			best = Info{Path: filepath.Join(dir, e.Name()), Size: info.Size(), At: info.ModTime()}
		}
	}
	return best, best.Path != ""
}

// list returns the scheduled backups in dir, oldest first (the names sort
// by time).
func list(dir string) []Info {
	entries, _ := os.ReadDir(dir)
	var out []Info
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), scheduledPrefix) || !strings.HasSuffix(e.Name(), scheduledSuffix) {
			continue
		}
		if info, err := e.Info(); err == nil {
			out = append(out, Info{Path: filepath.Join(dir, e.Name()), Size: info.Size(), At: info.ModTime()})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}
