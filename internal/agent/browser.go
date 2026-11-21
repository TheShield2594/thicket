package agent

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/theshield2594/thicket/internal/browser"
	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/store"
)

// A sprig's own web browser (docs/browser.md). The thicket-browser plugin
// runs one Chromium per sprig behind an egress proxy that enforces the
// allowlist; core keeps who may do what:
//
//   - The grant: mcp:browser/* like any plugin; kid sprigs are refused in
//     code (plugin.Browser isn't KidsAllowed), and again here on every
//     call.
//   - The allowlist: the sites the sprig's browser may reach. Only the
//     sprig's owner changes it (chat, API, dashboard); every change is in
//     the trail and the latest can be undone. Each call carries it as it
//     is at that moment, so a removal counts at once.
//   - The profile key: each sprig's profile is sealed at rest with its own
//     key, made here the first time and kept in the encrypted secret
//     store; it is sent with each call and the plugin never writes it.

// browserMeta is what a call to the browser plugin carries besides who is
// calling: the sprig's allowlist and its profile key.
func (e *Engine) browserMeta(ctx context.Context, sprig string) (map[string]any, error) {
	c, ok := e.Chars.Get(sprig)
	if !ok {
		return nil, errNoSuchSprig
	}
	if c.IsKid() {
		return nil, fmt.Errorf("%s is a kid sprig: kid sprigs can't use the browser", c.DisplayName())
	}
	allow, err := e.DB.BrowserAllow(ctx, sprig)
	if err != nil {
		return nil, fmt.Errorf("couldn't read %s's browser allowlist: %w", c.DisplayName(), err)
	}
	key, err := e.browserKey(ctx, sprig)
	if err != nil {
		return nil, fmt.Errorf("couldn't get %s's browser profile key: %w", c.DisplayName(), err)
	}
	return map[string]any{"allow": allow, "profile_key": key}, nil
}

// browserKey returns a sprig's profile key, making it the first time.
func (e *Engine) browserKey(ctx context.Context, sprig string) (string, error) {
	name := browser.ProfileKeySecret(sprig)
	e.browserMu.Lock()
	defer e.browserMu.Unlock()
	stored, err := e.Secrets.Stored(ctx, name)
	if err != nil {
		return "", err
	}
	if stored {
		return e.Secrets.Get(ctx, name)
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	key := base64.StdEncoding.EncodeToString(b)
	if err := e.Secrets.Put(ctx, name, key); err != nil {
		return "", err
	}
	return key, nil
}

// MaySeeBrowser reports whether u may see sprig c's browser allowlist:
// a private sprig's only its person, a shared sprig's any adult. Kid
// sprigs have no browser, and kids see none.
func MaySeeBrowser(u *store.User, c *character.Character) bool { return MayReadMail(u, c) }

// MaySeeBrowserOf is MaySeeBrowser by sprig id (the dashboard).
func (e *Engine) MaySeeBrowserOf(u *store.User, sprig string) bool {
	c, ok := e.Chars.Get(sprig)
	return ok && !c.PrivateFrom(u.ID) && MaySeeBrowser(u, c)
}

// BrowserAllowlist returns a sprig's browser allowlist for someone who
// may see it.
func (e *Engine) BrowserAllowlist(ctx context.Context, u *store.User, sprig string) ([]string, error) {
	c, ok := e.Chars.Get(sprig)
	if !ok || c.PrivateFrom(u.ID) {
		return nil, errNoSuchSprig
	}
	if !MaySeeBrowser(u, c) {
		return nil, ErrNotYours
	}
	list, err := e.DB.BrowserAllow(ctx, c.ID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", store.ErrStorage, err)
	}
	return list, nil
}

// CanManageBrowser reports whether u may change sprig's browser allowlist.
func (e *Engine) CanManageBrowser(ctx context.Context, u *store.User, sprig string) bool {
	c, ok := e.Chars.Get(sprig)
	return ok && !c.IsKid() && e.mayManage(ctx, u, c, "change the browser allowlist") == nil
}

// ChangeBrowserAllow adds and removes sites on sprig's browser allowlist.
// Only its owner may (mayManage); the change is in the trail, with an
// undo.
func (e *Engine) ChangeBrowserAllow(ctx context.Context, by *store.User, sprig string, add, remove []string, via string) ([]string, error) {
	c, ok := e.Chars.Get(sprig)
	if !ok {
		return nil, errNoSuchSprig
	}
	if c.IsKid() {
		return nil, fmt.Errorf("%s is a kid sprig: kid sprigs can't use the browser", c.DisplayName())
	}
	if err := e.mayManage(ctx, by, c, "change the browser allowlist"); err != nil {
		return nil, err
	}
	norm := func(in []string) ([]string, error) {
		var out []string
		for _, s := range in {
			if strings.TrimSpace(s) == "" {
				continue
			}
			v, err := browser.AllowEntry(s)
			if err != nil {
				return nil, err
			}
			if !slices.Contains(out, v) {
				out = append(out, v)
			}
		}
		return out, nil
	}
	a, err := norm(add)
	if err != nil {
		return nil, err
	}
	r, err := norm(remove)
	if err != nil {
		return nil, err
	}
	if len(a)+len(r) > 100 {
		return nil, errors.New("at most 100 changes at a time")
	}
	e.browserMu.Lock()
	defer e.browserMu.Unlock()
	var summary string
	_, _, err = e.DB.ChangeBrowserAllow(ctx, c.ID, a, r, func(added, removed []string) *store.TrailEntry {
		var parts []string
		if len(added) > 0 {
			parts = append(parts, "added "+strings.Join(added, ", "))
		}
		if len(removed) > 0 {
			parts = append(parts, "removed "+strings.Join(removed, ", "))
		}
		summary = strings.Join(parts, "; ")
		undo := trailUndo{Type: "browserallow", Character: c.ID, Tools: removed}
		undo.Args, _ = json.Marshal(added) // undo takes off what was added and puts back what was removed
		raw, _ := json.Marshal(undo)
		return &store.TrailEntry{UserID: by.ID, Character: c.ID, Kind: store.TrailAccess,
			Summary: fmt.Sprintf("%s's browser allowlist: %s (%s)", c.DisplayName(), summary, via), Undo: raw}
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", store.ErrStorage, err)
	}
	if summary != "" {
		e.logActivity(ctx, "info", "approval", c.ID, by.ID, "browser allowlist changed: "+summary, "")
	}
	list, err := e.DB.BrowserAllow(ctx, c.ID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", store.ErrStorage, err)
	}
	return list, nil
}

// undoBrowserAllow reverses an allowlist change (UndoTrail checked who,
// and that it is the latest, under browserMu).
func (e *Engine) undoBrowserAllow(ctx context.Context, u trailUndo) (string, error) {
	var added []string
	_ = json.Unmarshal(u.Args, &added)
	if _, _, err := e.DB.ChangeBrowserAllow(ctx, u.Character, u.Tools, added, nil); err != nil {
		return "", err
	}
	var parts []string
	if len(added) > 0 {
		parts = append(parts, strings.Join(added, ", ")+" off the browser allowlist")
	}
	if len(u.Tools) > 0 {
		parts = append(parts, strings.Join(u.Tools, ", ")+" back on it")
	}
	return fmt.Sprintf("%s: %s", u.Character, strings.Join(parts, "; ")), nil
}

// latestBrowserAllowChange is the newest browser allowlist change for a
// sprig still in force (not undone), or 0.
func (e *Engine) latestBrowserAllowChange(ctx context.Context, sprig string) (int64, error) {
	entries, err := e.DB.TrailOfKind(ctx, store.TrailAccess, sprig, 200)
	if err != nil {
		return 0, err
	}
	for _, t := range entries {
		var u trailUndo
		if json.Unmarshal(t.Undo, &u) == nil && u.Type == "browserallow" {
			return t.ID, nil
		}
	}
	return 0, nil
}

func (e *Engine) toolBrowserAllowlist(ctx context.Context, t *turn, args json.RawMessage) (string, error) {
	a, err := decode[struct {
		Sprig  string   `json:"sprig"`
		Add    []string `json:"add"`
		Remove []string `json:"remove"`
	}](args)
	if err != nil {
		return "", err
	}
	c, err := e.findSprig(t, a.Sprig)
	if err != nil {
		return "", err
	}
	var list []string
	if len(a.Add)+len(a.Remove) == 0 {
		if list, err = e.BrowserAllowlist(ctx, t.user, c.ID); err != nil {
			return "", err
		}
	} else if list, err = e.ChangeBrowserAllow(ctx, t.user, c.ID, a.Add, a.Remove, "in chat"); err != nil {
		return "", err
	}
	if len(list) == 0 {
		return fmt.Sprintf("%s's browser allowlist is empty: its browser can't load any site until its owner allows some.", c.DisplayName()), nil
	}
	note := ""
	if !c.AllowsMCP(browser.Plugin, "open") {
		note = fmt.Sprintf(" (%s doesn't have the browser yet: grant_tools mcp:browser/*.)", c.DisplayName())
	}
	return fmt.Sprintf("%s's browser may load these sites and their subdomains: %s. Everything else is blocked.%s", c.DisplayName(), strings.Join(list, ", "), note), nil
}
