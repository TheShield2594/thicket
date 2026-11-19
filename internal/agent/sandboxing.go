package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/theshield2594/thicket/internal/access"
	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/store"
)

// Per-sprig sandboxing, enforced in code:
//
//   - Exec: tools from an integration marked exec (one that runs commands,
//     like the sandbox computer) are offered only to sprigs whose owner
//     turned exec on, and refused when called without it.
//   - Files: a sprig's file access is its own folder (characters/<id>/)
//     plus paths its owner granted. A local (stdio) integration runs a
//     process of its own for each sprig, in that folder, and every
//     path-like argument it is called with is resolved (symlinks, "..")
//     and checked. Thicket's own data is never reachable.

// SprigFolder is a sprig's own folder.
func (e *Engine) SprigFolder(id string) string {
	return filepath.Join(e.Chars.Dir(), id)
}

// protectedPaths is Thicket's own data, which no grant can reach: the data
// folder (database, keys, backups), every sprig's folder and file, and
// the key file wherever it is kept.
func (e *Engine) protectedPaths() []string {
	var out []string
	for _, p := range []string{e.Cfg.DataDir, e.Chars.Dir(), os.Getenv("THICKET_SECRET_KEY_FILE")} {
		if p == "" {
			continue
		}
		if abs, err := filepath.Abs(p); err == nil {
			out = append(out, abs)
		}
	}
	return out
}

// filePolicy is a sprig's file access as it stands now.
func (e *Engine) filePolicy(ctx context.Context, c *character.Character) (access.Policy, error) {
	grants, err := e.DB.PathGrants(ctx, c.ID)
	if err != nil {
		return access.Policy{}, err
	}
	folder, err := filepath.Abs(e.SprigFolder(c.ID))
	if err != nil {
		return access.Policy{}, err
	}
	if c.IsKid() {
		grants = nil // kid sprigs never get more than their own folder
	}
	return access.Policy{Folder: folder, Grants: grants, Protected: e.protectedPaths()}, nil
}

// execAllowed reports whether a sprig may use exec-capable tools. It
// fails closed.
func (e *Engine) execAllowed(ctx context.Context, c *character.Character) bool {
	if c.IsKid() {
		return false
	}
	ok, err := e.DB.ExecAllowed(ctx, c.ID)
	if err != nil {
		e.Log.Error("read exec access; refusing", "sprig", c.ID, "err", err)
		return false
	}
	return ok
}

// pathKeys are argument names that hold paths on the integration's
// machine. Values under them (strings, or lists of strings, at any depth)
// are checked for local integrations.
var pathKeys = map[string]bool{
	"path": true, "paths": true, "file": true, "files": true, "filename": true, "filepath": true,
	"file_path": true, "filepaths": true, "dir": true, "directory": true, "folder": true,
	"source": true, "destination": true, "src": true, "dst": true, "dest": true, "root": true,
	"cwd": true, "workdir": true, "working_directory": true, "target_path": true,
	"source_path": true, "destination_path": true, "output": true, "output_path": true, "input_path": true,
}

// pathArgs collects the values of path-like arguments.
func pathArgs(args json.RawMessage) []string {
	var v any
	if json.Unmarshal(args, &v) != nil {
		return nil
	}
	var out []string
	var walk func(v any, pathy bool)
	walk = func(v any, pathy bool) {
		switch x := v.(type) {
		case map[string]any:
			for k, val := range x {
				walk(val, pathKeys[strings.ToLower(k)])
			}
		case []any:
			for _, val := range x {
				walk(val, pathy)
			}
		case string:
			if pathy {
				out = append(out, x)
			}
		}
	}
	walk(v, false)
	return out
}

// checkLocalPaths refuses a call to a local integration whose path
// arguments leave the sprig's files.
func (e *Engine) checkLocalPaths(ctx context.Context, c *character.Character, args json.RawMessage) error {
	paths := pathArgs(args)
	if len(paths) == 0 {
		return nil
	}
	pol, err := e.filePolicy(ctx, c)
	if err != nil {
		return fmt.Errorf("couldn't read %s's file access, so nothing was done: %w", c.DisplayName(), err)
	}
	for _, p := range paths {
		if strings.HasPrefix(p, "file://") {
			p = strings.TrimPrefix(p, "file://")
		}
		if _, err := pol.Check(p); err != nil {
			return fmt.Errorf("%s can only use files in its own folder and paths its owner granted, and %q isn't one of them, so nothing was done", c.DisplayName(), p)
		}
	}
	return nil
}

// --- the set_sprig_access tool ---

func (e *Engine) toolSetSprigAccess(ctx context.Context, t *turn, args json.RawMessage) (string, error) {
	a, err := decode[struct {
		Sprig      string `json:"sprig"`
		Exec       *bool  `json:"exec"`
		GrantPath  string `json:"grant_path"`
		RevokePath string `json:"revoke_path"`
	}](args)
	if err != nil {
		return "", err
	}
	c, err := e.findSprig(nil, a.Sprig)
	if err == nil && c.PrivateFrom(t.user.ID) {
		err = fmt.Errorf("there's no sprig called %q", a.Sprig)
	}
	if err != nil {
		return "", err
	}
	describe := func() string {
		exec := "can't run commands"
		if e.execAllowed(ctx, c) {
			exec = "may run commands (exec is on)"
		}
		grants, _ := e.DB.PathGrants(ctx, c.ID)
		paths := "only its own folder"
		if len(grants) > 0 {
			paths = "its own folder and " + strings.Join(grants, ", ")
		}
		return fmt.Sprintf("%s %s, and can use files in %s.", c.DisplayName(), exec, paths)
	}
	if a.Exec == nil && a.GrantPath == "" && a.RevokePath == "" {
		return describe(), nil
	}
	if err := e.mayManage(ctx, t.user, c, "change what it may run or read"); err != nil {
		return "", err
	}
	if a.Exec != nil {
		if err := e.SetExec(ctx, t.user, c, *a.Exec); err != nil {
			return "", err
		}
	}
	if p := strings.TrimSpace(a.GrantPath); p != "" {
		if err := e.GrantPath(ctx, t.user, c, p); err != nil {
			return "", err
		}
	}
	if p := strings.TrimSpace(a.RevokePath); p != "" {
		if err := e.RevokePath(ctx, t.user, c, p); err != nil {
			return "", err
		}
	}
	return describe(), nil
}

// SetExec turns a sprig's exec opt-in on or off, for its owner. Recorded
// in the trail, with an undo.
func (e *Engine) SetExec(ctx context.Context, by *store.User, c *character.Character, on bool) error {
	if err := e.mayManage(ctx, by, c, "change whether it may run commands"); err != nil {
		return err
	}
	if c.IsKid() && on {
		return fmt.Errorf("%s is a kid sprig: it can never run commands", c.DisplayName())
	}
	// The stored value itself, read errors included: execAllowed's
	// fail-closed "no" would make turning exec off a silent no-op.
	prev, err := e.DB.ExecAllowed(ctx, c.ID)
	if err != nil {
		return fmt.Errorf("couldn't read %s's exec setting: %w", c.DisplayName(), err)
	}
	if prev == on {
		return nil
	}
	if err := e.DB.SetExecAllowed(ctx, c.ID, on, userID(by)); err != nil {
		return err
	}
	state := map[bool]string{true: "on", false: "off"}
	e.addTrail(ctx, userID(by), c.ID, store.TrailAccess, fmt.Sprintf("%s's exec: %s → %s", c.DisplayName(), state[prev], state[on]), "",
		&trailUndo{Type: "exec", Character: c.ID, Level: state[prev]})
	return nil
}

// GrantPath lets a sprig use a path beyond its own folder, for its owner.
func (e *Engine) GrantPath(ctx context.Context, by *store.User, c *character.Character, path string) error {
	if err := e.mayManage(ctx, by, c, "grant it a path"); err != nil {
		return err
	}
	if c.IsKid() {
		return fmt.Errorf("%s is a kid sprig: it only ever gets its own folder", c.DisplayName())
	}
	r, err := access.CheckGrant(path, e.protectedPaths())
	if err != nil {
		return err
	}
	added, err := e.DB.GrantPath(ctx, c.ID, r, userID(by))
	if err != nil || !added {
		return err
	}
	e.addTrail(ctx, userID(by), c.ID, store.TrailAccess, fmt.Sprintf("%s may use %s", c.DisplayName(), r), "",
		&trailUndo{Type: "path", Character: c.ID, Tool: r, Level: "revoke"})
	return nil
}

// RevokePath takes a granted path away, for the sprig's owner.
func (e *Engine) RevokePath(ctx context.Context, by *store.User, c *character.Character, path string) error {
	if err := e.mayManage(ctx, by, c, "take a path away"); err != nil {
		return err
	}
	r := path
	if res, err := access.Resolve("", path); err == nil && filepath.IsAbs(path) {
		r = res
	}
	removed, err := e.DB.RevokePath(ctx, c.ID, r)
	if err == nil && !removed && r != path {
		removed, err = e.DB.RevokePath(ctx, c.ID, path) // stored before its target moved
		r = path
	}
	if err != nil {
		return err
	}
	if !removed {
		return fmt.Errorf("%s wasn't granted %s", c.DisplayName(), path)
	}
	e.addTrail(ctx, userID(by), c.ID, store.TrailAccess, fmt.Sprintf("%s may no longer use %s", c.DisplayName(), r), "",
		&trailUndo{Type: "path", Character: c.ID, Tool: r, Level: "grant"})
	return nil
}

// undoAccess reverses an exec or path change.
func (e *Engine) undoAccess(ctx context.Context, by *store.User, u trailUndo) (string, error) {
	c, ok := e.Chars.Get(u.Character)
	if !ok {
		return "", fmt.Errorf("the sprig %q no longer exists", u.Character)
	}
	switch {
	case u.Type == "exec":
		on := u.Level == "on"
		if c.IsKid() && on {
			return "", errors.New("kid sprigs can't run commands")
		}
		if err := e.DB.SetExecAllowed(ctx, c.ID, on, userID(by)); err != nil {
			return "", err
		}
		return fmt.Sprintf("%s's exec is %s again", c.DisplayName(), u.Level), nil
	case u.Level == "revoke":
		if _, err := e.DB.RevokePath(ctx, c.ID, u.Tool); err != nil {
			return "", err
		}
		return fmt.Sprintf("%s may no longer use %s", c.DisplayName(), u.Tool), nil
	default:
		r, err := access.CheckGrant(u.Tool, e.protectedPaths())
		if err != nil {
			return "", err
		}
		if _, err := e.DB.GrantPath(ctx, c.ID, r, userID(by)); err != nil {
			return "", err
		}
		return fmt.Sprintf("%s may use %s again", c.DisplayName(), r), nil
	}
}

func userID(u *store.User) int64 {
	if u == nil {
		return 0
	}
	return u.ID
}
