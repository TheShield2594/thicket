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
	Format        int       `json:"format"`
	SchemaVersion int       `json:"schema_version"`
	CreatedAt     time.Time `json:"created_at"`
	IncludesKey   bool      `json:"includes_key"`
}

// Export writes a .tar.gz with the database, character files and avatars.
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
	man, _ := json.MarshalIndent(Manifest{Format: 1, SchemaVersion: ver, CreatedAt: time.Now().UTC(), IncludesKey: includeKey}, "", "  ")
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
			// A sprig's folder: its voice tests travel with it.
			tests, _ := filepath.Glob(filepath.Join(charsDir, e.Name(), "voice_tests", "*.yaml"))
			for _, path := range tests {
				if err := addFile(tw, path, "characters/"+e.Name()+"/voice_tests/"+filepath.Base(path), 0o644); err != nil {
					return err
				}
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

// voiceTestPath matches a sprig's voice test in an archive.
// The file name may be anything Export includes (any *.yaml name, hidden
// ones too) except separators and control characters; ".." is refused
// separately.
var voiceTestPath = regexp.MustCompile(`^characters/[a-z0-9][a-z0-9_-]*/voice_tests/[^/\\\x00-\x1f]+\.yaml$`)

// maxEntry bounds any single file in an archive.
const maxEntry int64 = 64 << 30

// Restore unpacks an export into dataDir. The archive is extracted into a
// staging directory and checked first; only then are the installation's
// files swapped in. Existing files are moved aside (never deleted), and if
// anything fails part-way the swap is undone so the database and key stay
// consistent. An existing database is only replaced with force.
func Restore(archive io.Reader, dataDir, charsDir string, force bool) error {
	dbPath := filepath.Join(dataDir, "thicket.db")
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
		case voiceTestPath.MatchString(filepath.ToSlash(name)) && h.Typeflag == tar.TypeReg:
			parts := strings.Split(filepath.ToSlash(name), "/")
			dest = filepath.Join(charsDir, parts[1], "voice_tests", parts[3])
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
		return errors.New("archive has no thicket.db; is this a Thicket export?")
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
