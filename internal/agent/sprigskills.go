package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/store"
)

// A sprig's own skills are taught to it (save_skill with for: this_sprig)
// or written by hand, and offered only to it. They live in its folder,
// characters/<id>/skills/<skill id>.md, which is the source of truth like
// voice_tests/: SyncSprigSkills mirrors the folder into the store whenever
// the characters load, so versions, history and runs work as they do for
// the shared library. Changes made in chat are written to the file first
// thing after they're saved.

// sprigSkillPath is a sprig's skill file.
func (e *Engine) sprigSkillPath(sprig, id string) string {
	return filepath.Join(character.SkillDir(e.Chars.Dir(), sprig), id+".md")
}

// writeSprigSkill writes a sprig's own skill to its folder, atomically.
func (e *Engine) writeSprigSkill(k store.Skill) error {
	b, err := FormatSkill(k)
	if err != nil {
		return err
	}
	path := e.sprigSkillPath(k.SprigID, k.ID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".skill-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// removeSprigSkill deletes a sprig's skill file (gone already is fine).
func (e *Engine) removeSprigSkill(sprig, id string) error {
	if err := os.Remove(e.sprigSkillPath(sprig, id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// retireSprigSkill keeps a promoted skill's file in the sprig's folder
// under a name the loader skips (<id>.md.promoted), so nothing written by
// hand is lost but the sprig doesn't own it again on the next load.
func (e *Engine) retireSprigSkill(sprig, id string) error {
	path := e.sprigSkillPath(sprig, id)
	if err := os.Rename(path, path+".promoted"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// SkillSyncProblem is a sprig skill file that couldn't be mirrored.
type SkillSyncProblem struct {
	Path    string
	Problem string
}

func (p SkillSyncProblem) Error() string {
	return filepath.Base(filepath.Dir(filepath.Dir(p.Path))) + "/skills/" + filepath.Base(p.Path) + ": " + p.Problem
}

// SyncSprigSkills mirrors every loaded sprig's skills folder into the
// store: a new file becomes a skill owned by that sprig, a changed file a
// new version, and a removed file deletes the skill (its versions are
// kept). A sprig whose character file is gone loses its skills too. Files
// that can't be mirrored are skipped and returned, and the store keeps
// what it had for them.
func (e *Engine) SyncSprigSkills(ctx context.Context) []SkillSyncProblem {
	e.skillSyncMu.Lock()
	defer e.skillSyncMu.Unlock()
	var probs []SkillSyncProblem
	owners, err := e.DB.SkillOwners(ctx)
	if err != nil {
		e.Log.Error("sync sprig skills", "err", err)
		return nil
	}
	dir := e.Chars.Dir()
	seen := map[string]bool{} // skill ids that have a file
	keep := map[string]bool{} // sprigs whose folder couldn't be read: keep their skills
	for _, c := range e.Chars.All() {
		paths, err := sprigSkillFiles(character.SkillDir(dir, c.ID))
		if err != nil {
			probs = append(probs, SkillSyncProblem{character.SkillDir(dir, c.ID), "couldn't read the folder: " + err.Error()})
			keep[c.ID] = true
			continue
		}
		for _, path := range paths {
			k, err := readSprigSkill(path)
			if err != nil {
				probs = append(probs, SkillSyncProblem{path, err.Error()})
				if k.ID != "" && owners[k.ID] == c.ID {
					seen[k.ID] = true // keep the last good version
				}
				continue
			}
			k.SprigID = c.ID
			if owner, ok := owners[k.ID]; ok && owner != c.ID {
				probs = append(probs, SkillSyncProblem{path, fmt.Sprintf("%s already has a skill with the id %q: rename one", e.charName(owner), k.ID)})
				continue
			}
			seen[k.ID] = true
			base := 0
			cur, err := e.DB.SkillByID(ctx, k.ID, 0)
			switch {
			case err == nil && cur.Shared():
				probs = append(probs, SkillSyncProblem{path, fmt.Sprintf("the shared library already has a skill with the id %q: rename this one", k.ID)})
				delete(seen, k.ID)
				continue
			case err == nil:
				base = cur.Version
			case !errors.Is(err, store.ErrNotFound):
				e.Log.Error("sync sprig skills", "skill", k.ID, "err", err)
				continue
			}
			k.Author, k.Note = "folder", "from "+c.ID+"/skills/"+filepath.Base(path)
			v, err := e.DB.SaveSkill(ctx, k, base)
			switch {
			case errors.Is(err, store.ErrNoChange):
			case err != nil:
				probs = append(probs, SkillSyncProblem{path, err.Error()})
			default:
				owners[k.ID] = c.ID
				e.Log.Info("sprig skill loaded", "sprig", c.ID, "skill", k.ID, "version", v)
			}
		}
	}
	// Skills whose file is gone, or whose sprig is.
	for id, sprig := range owners {
		if seen[id] || keep[sprig] {
			continue
		}
		if _, ok := e.Chars.Get(sprig); !ok {
			// A character file that fails to load keeps its skills until
			// it's fixed; only a removed one takes them away.
			if _, err := os.Stat(filepath.Join(dir, sprig+".md")); err == nil {
				continue
			}
		}
		// Only while it's still that sprig's: one promoted meanwhile stays.
		if err := e.DB.DeleteSprigSkill(ctx, id, sprig); errors.Is(err, store.ErrNotFound) {
			continue
		} else if err != nil {
			e.Log.Error("sync sprig skills: delete", "skill", id, "err", err)
			continue
		}
		e.Log.Info("sprig skill removed", "sprig", sprig, "skill", id)
	}
	for _, p := range probs {
		e.Log.Error("sprig skill file problem", "file", p.Path, "problem", p.Problem)
	}
	return probs
}

// sprigSkillFiles lists a skills folder's *.md files, sorted. A missing
// folder is no files; any other read error is returned, so a folder that
// can't be read isn't taken for one that was emptied.
func sprigSkillFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, en := range entries {
		if n := en.Name(); !en.IsDir() && strings.HasSuffix(n, ".md") && !strings.HasPrefix(n, ".") {
			out = append(out, filepath.Join(dir, n))
		}
	}
	sort.Strings(out)
	return out, nil
}

// readSprigSkill parses one skill file; it must be named <id>.md. On a
// parse error the returned skill still carries the file's id, if the
// name gives one.
func readSprigSkill(path string) (store.Skill, error) {
	want := strings.TrimSuffix(filepath.Base(path), ".md")
	b, err := os.ReadFile(path)
	if err != nil {
		return store.Skill{ID: want}, err
	}
	k, err := ParseSkill(b)
	if err != nil {
		return store.Skill{ID: want}, err
	}
	if k.ID != want {
		return store.Skill{ID: want}, fmt.Errorf("id %q must match the file name (%s.md)", k.ID, k.ID)
	}
	return k, nil
}

// --- runs ---

func (e *Engine) toolUseSkill(ctx context.Context, t *turn, args json.RawMessage) (string, error) {
	a, err := decode[struct {
		ID string `json:"id"`
	}](args)
	if err != nil {
		return "", err
	}
	k, err := e.offeredSkill(ctx, t, a.ID, 0)
	if err != nil {
		return "", err
	}
	n, err := e.DB.SkillRunCount(ctx, k.ID, t.char.ID)
	if err != nil {
		return "", err
	}
	run, err := e.DB.StartSkillRun(ctx, store.SkillRun{SkillID: k.ID, Version: k.Version, SprigID: t.char.ID, UserID: t.user.ID, ChatRef: t.chatRef})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Run %d started (your run number %d of this skill). Follow the steps; when you've finished or can't go on, call finish_skill with run %d.\n%s",
		run, n+1, run, describeSkill(k)), nil
}

func (e *Engine) toolFinishSkill(ctx context.Context, t *turn, args json.RawMessage) (string, error) {
	a, err := decode[struct {
		Run     int64  `json:"run"`
		Outcome string `json:"outcome"`
		Note    string `json:"note"`
	}](args)
	if err != nil {
		return "", err
	}
	if a.Outcome != store.SkillSucceeded && a.Outcome != store.SkillFailed {
		return "", fmt.Errorf("outcome is %s or %s", store.SkillSucceeded, store.SkillFailed)
	}
	note := strings.TrimSpace(a.Note)
	if len(note) > 300 {
		note = note[:300]
	}
	if err := e.DB.FinishSkillRun(ctx, a.Run, t.char.ID, a.Outcome, note); errors.Is(err, store.ErrNotFound) {
		return "", fmt.Errorf("you have no run %d still going", a.Run)
	} else if err != nil {
		return "", err
	}
	return fmt.Sprintf("Run %d recorded as %s.", a.Run, a.Outcome), nil
}

// SkillRunCount is how many times a sprig has started following a skill,
// for rules that treat a skill's first runs differently.
func (e *Engine) SkillRunCount(ctx context.Context, skillID, sprig string) (int, error) {
	return e.DB.SkillRunCount(ctx, skillID, sprig)
}
