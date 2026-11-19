package character

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed defaults/*.md defaults/*/voice_tests/*.yaml defaults/*/avatar.json
var defaultsFS embed.FS

// HashStore remembers the hash of each file Thicket wrote, so a later
// write can tell whether the user has edited the file since.
type HashStore interface {
	CharacterFileHash(ctx context.Context, id string) (string, error)
	SetCharacterFileHash(ctx context.Context, id, hash string) error
}

// ErrUserEdited means a write was refused because the user changed the
// file by hand. The proposed version is saved next to it instead.
var ErrUserEdited = errors.New("character file was edited by hand")

// LoadError describes a character file that failed to load.
type LoadError struct {
	Path     string
	Problems []string
}

func (e LoadError) Error() string {
	return filepath.Base(e.Path) + ": " + strings.Join(e.Problems, "; ")
}

type Registry struct {
	dir    string
	hashes HashStore
	log    *slog.Logger

	writeMu sync.Mutex // serialises Write's check-then-write
	mu      sync.RWMutex
	chars   map[string]*Character
	errs    []LoadError
	mtimes  map[string]time.Time
	onLoad  []func()
}

// OnLoad registers f to run after every successful Load: a sprig was
// created, changed or removed. Register before the registry is shared.
func (r *Registry) OnLoad(f func()) { r.onLoad = append(r.onLoad, f) }

func NewRegistry(dir string, hashes HashStore, log *slog.Logger) *Registry {
	return &Registry{dir: dir, hashes: hashes, log: log, chars: map[string]*Character{}, mtimes: map[string]time.Time{}}
}

func (r *Registry) Dir() string { return r.dir }

// EnsureDefaults writes the bundled starter characters (Juniper) on first
// run. Existing files are never touched.
func (r *Registry) EnsureDefaults(ctx context.Context) error {
	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		return err
	}
	entries, err := defaultsFS.ReadDir("defaults")
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			if err := r.ensureDefaultVoiceTests(e.Name()); err != nil {
				return err
			}
			if err := r.ensureDefaultFile(e.Name(), "avatar.json"); err != nil {
				return err
			}
			continue
		}
		dest := filepath.Join(r.dir, e.Name())
		if _, err := os.Stat(dest); err == nil {
			continue
		}
		b, err := defaultsFS.ReadFile("defaults/" + e.Name())
		if err != nil {
			return err
		}
		if err := os.WriteFile(dest, b, 0o644); err != nil {
			return err
		}
		if err := r.hashes.SetCharacterFileHash(ctx, strings.TrimSuffix(e.Name(), ".md"), HashBytes(b)); err != nil {
			return err
		}
	}
	return nil
}

// ensureDefaultVoiceTests writes a bundled sprig's voice tests if it has
// no voice_tests folder yet. An existing folder is left alone.
func (r *Registry) ensureDefaultVoiceTests(id string) error {
	dir := VoiceTestDir(r.dir, id)
	if _, err := os.Stat(dir); err == nil {
		return nil
	}
	files, err := defaultsFS.ReadDir("defaults/" + id + "/voice_tests")
	if err != nil {
		return nil // no bundled tests for this one
	}
	// Written to a temporary folder and renamed into place once complete,
	// so a failure never leaves a half-filled folder that later starts
	// would take as finished.
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dir), ".voice_tests-")
	if err != nil {
		return err
	}
	for _, f := range files {
		b, err := defaultsFS.ReadFile("defaults/" + id + "/voice_tests/" + f.Name())
		if err == nil {
			err = os.WriteFile(filepath.Join(tmp, f.Name()), b, 0o644)
		}
		if err != nil {
			os.RemoveAll(tmp)
			return err
		}
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		os.RemoveAll(tmp)
		return err
	}
	if err := os.Rename(tmp, dir); err != nil {
		os.RemoveAll(tmp)
		return err
	}
	return nil
}

// ensureDefaultFile copies a bundled sprig's file into its folder if it
// isn't there yet.
func (r *Registry) ensureDefaultFile(id, name string) error {
	dest := filepath.Join(r.dir, id, name)
	if _, err := os.Stat(dest); err == nil {
		return nil
	}
	b, err := defaultsFS.ReadFile("defaults/" + id + "/" + name)
	if err != nil {
		return nil // not bundled
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dest, b, 0o644)
}

// Load (re)reads every *.md file in the directory. Invalid files are
// skipped and reported via Errors; valid ones replace the current set.
func (r *Registry) Load() error {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return err
	}
	chars := map[string]*Character{}
	var errs []LoadError
	mtimes := map[string]time.Time{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") || strings.HasSuffix(name, ".proposed.md") || strings.HasPrefix(name, ".") {
			continue
		}
		path := filepath.Join(r.dir, name)
		if info, err := e.Info(); err == nil {
			mtimes[path] = info.ModTime()
		}
		b, err := os.ReadFile(path)
		if err != nil {
			errs = append(errs, LoadError{path, []string{err.Error()}})
			continue
		}
		c, err := Parse(b)
		if err != nil {
			errs = append(errs, LoadError{path, []string{err.Error()}})
			continue
		}
		c.Path = path
		if probs := c.Validate(); len(probs) > 0 {
			errs = append(errs, LoadError{path, probs})
			continue
		}
		if want := strings.TrimSuffix(name, ".md"); c.ID != want {
			errs = append(errs, LoadError{path, []string{fmt.Sprintf("id %q must match the file name (%s.md)", c.ID, c.ID)}})
			continue
		}
		chars[c.ID] = c
	}
	var hosts []string
	for id, c := range chars {
		if c.Host {
			hosts = append(hosts, id)
		}
	}
	sort.Strings(hosts)
	if len(hosts) == 0 {
		errs = append(errs, LoadError{r.dir, []string{"no character has `host: true`; Juniper's file may be missing or broken"}})
	} else if len(hosts) > 1 {
		// Keep the first; demote the rest so routing stays predictable.
		for _, id := range hosts[1:] {
			chars[id].Host = false
		}
		errs = append(errs, LoadError{r.dir, []string{"more than one host character (" + strings.Join(hosts, ", ") + "); using " + hosts[0]}})
	}
	r.mu.Lock()
	r.chars, r.errs, r.mtimes = chars, errs, mtimes
	r.mu.Unlock()
	for _, e := range errs {
		r.log.Error("character file problem", "file", e.Path, "problems", strings.Join(e.Problems, "; "))
	}
	for _, f := range r.onLoad {
		f()
	}
	return nil
}

// Changed reports whether any file in the directory was added, removed or
// modified since the last Load. Cheap polling instead of a watcher dep.
func (r *Registry) Changed() bool {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	seen := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") || strings.HasSuffix(name, ".proposed.md") || strings.HasPrefix(name, ".") {
			continue
		}
		seen++
		info, err := e.Info()
		if err != nil {
			return true
		}
		if old, ok := r.mtimes[filepath.Join(r.dir, name)]; !ok || !old.Equal(info.ModTime()) {
			return true
		}
	}
	return seen != len(r.mtimes)
}

// Watch polls for changes and reloads, calling onReload afterwards.
func (r *Registry) Watch(ctx context.Context, every time.Duration, onReload func([]LoadError)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if r.Changed() {
				if err := r.Load(); err != nil {
					r.log.Error("reload characters", "err", err)
					continue
				}
				r.log.Info("characters reloaded", "count", len(r.All()))
				if onReload != nil {
					onReload(r.Errors())
				}
			}
		}
	}
}

func (r *Registry) Get(id string) (*Character, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	c, ok := r.chars[id]
	return c, ok
}

// PrivateFrom reports whether the sprig id is someone else's solo sprig,
// as seen by userID (see Character.PrivateFrom). Unknown ids aren't.
func (r *Registry) PrivateFrom(id string, userID int64) bool {
	c, ok := r.Get(id)
	return ok && c.PrivateFrom(userID)
}

// For returns the sprigs a person may talk to, host first.
func (r *Registry) For(p Person) []*Character {
	var out []*Character
	for _, c := range r.All() {
		if c.UsableBy(p) {
			out = append(out, c)
		}
	}
	return out
}

// All returns characters sorted with the host first, then by name.
func (r *Registry) All() []*Character {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Character, 0, len(r.chars))
	for _, c := range r.chars {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Host != out[j].Host {
			return out[i].Host
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func (r *Registry) Host() (*Character, bool) {
	for _, c := range r.All() {
		if c.Host {
			return c, true
		}
	}
	return nil, false
}

func (r *Registry) Errors() []LoadError {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]LoadError(nil), r.errs...)
}

// FindByName returns the character answering to name (case-insensitive).
func (r *Registry) FindByName(name string) (*Character, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, c := range r.All() {
		if c.ID == name {
			return c, true
		}
		for _, n := range c.Names() {
			if n == name {
				return c, true
			}
		}
	}
	return nil, false
}

// Write saves a character file. It never overwrites a file the user has
// edited by hand: in that case the new version goes to <id>.proposed.md and
// ErrUserEdited is returned. The registry is reloaded on success.
func (r *Registry) Write(ctx context.Context, c *Character) (string, error) {
	if probs := c.Validate(); len(probs) > 0 {
		return "", fmt.Errorf("character is not valid: %s", strings.Join(probs, "; "))
	}
	data, err := Marshal(c)
	if err != nil {
		return "", err
	}
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	path := filepath.Join(r.dir, c.ID+".md")
	if existing, err := os.ReadFile(path); err == nil {
		known, err := r.hashes.CharacterFileHash(ctx, c.ID)
		if err != nil {
			return "", err
		}
		if known == "" || known != HashBytes(existing) {
			proposed := filepath.Join(r.dir, c.ID+".proposed.md")
			if err := writeAtomic(proposed, data); err != nil {
				return "", err
			}
			return proposed, ErrUserEdited
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := writeAtomic(path, data); err != nil {
		return "", err
	}
	if err := r.hashes.SetCharacterFileHash(ctx, c.ID, HashBytes(data)); err != nil {
		return "", err
	}
	return path, r.Load()
}

func writeAtomic(path string, data []byte) error {
	// A unique temp file in the same directory, so concurrent writers can't
	// clobber each other's half-written file before the rename.
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, 0o644); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
