// Package backup exports and restores a Thicket install (database plus
// character files) and shares individual characters.
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
	"sort"
	"strings"
	"time"

	"github.com/theshield2594/thicket/internal/character"
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
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") || strings.HasSuffix(e.Name(), ".tmp") {
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

// Restore unpacks an export into dataDir. It refuses to overwrite an
// existing database unless force is set, in which case the old files are
// moved aside rather than deleted.
func Restore(archive io.Reader, dataDir, charsDir string, force bool) error {
	dbPath := filepath.Join(dataDir, "thicket.db")
	if _, err := os.Stat(dbPath); err == nil && !force {
		return fmt.Errorf("%s already exists; stop Thicket and pass --force to replace it (the current files are kept as .before-restore)", dbPath)
	}
	gz, err := gzip.NewReader(archive)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	suffix := ".before-restore-" + time.Now().UTC().Format("20060102T150405")
	sawDB := false
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
		case name == "manifest.json":
			continue
		case name == "thicket.db":
			dest, sawDB = dbPath, true
			for _, ext := range []string{"", "-wal", "-shm"} {
				if _, err := os.Stat(dbPath + ext); err == nil {
					if err := os.Rename(dbPath+ext, dbPath+ext+suffix); err != nil {
						return err
					}
				}
			}
		case name == "secret.key":
			dest = filepath.Join(dataDir, "secret.key")
		case strings.HasPrefix(name, "characters/"):
			dest = filepath.Join(charsDir, filepath.Base(name))
		default:
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
			return err
		}
		if _, err := os.Stat(dest); err == nil && dest != dbPath {
			if err := os.Rename(dest, dest+suffix); err != nil {
				return err
			}
		}
		f, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(h.Mode)&0o644|0o600)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, io.LimitReader(tr, 4<<30)); err != nil {
			f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
	if !sawDB {
		return errors.New("archive has no thicket.db; is this a Thicket export?")
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

// ImportCharacter copies a shared character file into the characters
// directory. For safety, an imported sprig keeps only the "schedule" scope:
// integrations and admin powers must be granted explicitly by the owner.
func ImportCharacter(src, charsDir string) (*character.Character, []string, error) {
	b, err := os.ReadFile(src)
	if err != nil {
		return nil, nil, err
	}
	c, err := character.Parse(b)
	if err != nil {
		return nil, nil, err
	}
	if probs := c.Validate(); len(probs) > 0 {
		return nil, nil, fmt.Errorf("invalid character: %s", strings.Join(probs, "; "))
	}
	dest := filepath.Join(charsDir, c.ID+".md")
	if _, err := os.Stat(dest); err == nil {
		return nil, nil, fmt.Errorf("a character with id %q already exists (%s)", c.ID, dest)
	}
	var stripped []string
	var kept []string
	for _, t := range c.Tools {
		if t == character.ScopeSchedule {
			kept = append(kept, t)
		} else {
			stripped = append(stripped, t)
		}
	}
	c.Tools = kept
	if c.Host {
		c.Host = false
		stripped = append(stripped, "host")
	}
	c.CreatedBy = "import"
	out, err := character.Marshal(c)
	if err != nil {
		return nil, nil, err
	}
	if err := os.WriteFile(dest, out, 0o644); err != nil {
		return nil, nil, err
	}
	return c, stripped, nil
}
