package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/theshield2594/thicket/internal/sprigpkg"
	"github.com/theshield2594/thicket/internal/store"
)

// Gallery packages sent in chat (`thicket export-sprig --for-gallery`
// files) go through sprigpkg.ReadGallery's strict checks, signature first.
// The person sees the author key's fingerprint and whether it's known,
// every file in full and every scope the sprig asks for; nothing is
// written until they approve. An update of a sprig that came from the
// gallery needs the same author's key and someone who may manage it.

// readGallery reads, checks and prepares a gallery package for user.
func (e *Engine) readGallery(ctx context.Context, user *store.User, raw []byte, id, name string) (*sprigpkg.Gallery, sprigpkg.Trust, error) {
	var trust sprigpkg.Trust
	revoked, err := sprigpkg.RevokedKeys(ctx, e.DB)
	if err != nil {
		return nil, trust, err
	}
	g, err := sprigpkg.ReadGallery(bytes.NewReader(raw), revoked)
	if err != nil {
		return nil, trust, err
	}
	if id = strings.ToLower(strings.TrimSpace(id)); id != "" {
		g.Char.ID = id
	}
	if trust, err = g.CheckTrust(ctx, e.DB, e.Chars.Dir(), revoked); err != nil {
		return nil, trust, err
	}
	if err := g.Prepare("", name, trust.Update); err != nil {
		return nil, trust, err
	}
	if trust.Update {
		cur, ok := e.Chars.Get(g.Char.ID)
		if !ok {
			return nil, trust, fmt.Errorf("%s can't be read right now, so it can't be updated", g.Char.ID)
		}
		if cur.IsKid() {
			return nil, trust, errors.New("kid sprigs can't have gallery imports")
		}
		if err := e.mayManage(ctx, user, cur, "update a sprig from the gallery"); err != nil {
			return nil, trust, err
		}
	} else {
		taken := func(s string) bool {
			if _, ok := e.Chars.Get(strings.ToLower(s)); ok {
				return true
			}
			_, ok := e.Chars.FindByName(s)
			return ok
		}
		if err := sprigpkg.CheckClash(g.Char, e.Chars.Dir(), taken); err != nil {
			return nil, trust, err
		}
	}
	if err := g.PlanSkills(ctx, e.DB, ValidateSkill); err != nil {
		return nil, trust, err
	}
	return g, trust, nil
}

// proposeGalleryImport shows a gallery package and asks to import it.
func (e *Engine) proposeGalleryImport(ctx context.Context, t *turn, raw []byte, file, id, name string) (string, error) {
	g, trust, err := e.readGallery(ctx, t.user, raw, id, name)
	if err != nil {
		var c *sprigpkg.Clash
		if errors.As(err, &c) {
			return "", fmt.Errorf("%w: ask the user what %s to import it under (suggest %q) and call this again with the same file and that %s", err, c.What, c.Suggest, c.What)
		}
		return "", fmt.Errorf("this gallery package can't be imported: %w", err)
	}
	preview, err := g.Preview(trust)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(sprigImportArgs{File: file, ID: id, Name: name, Package: base64.StdEncoding.EncodeToString(raw),
		Gallery: &galleryShown{Sprig: g.Char.ID, Update: trust.Update, Skills: g.SkillIDs()}})
	if err != nil {
		return "", err
	}
	who := g.Char.DisplayName()
	verb := "import"
	if trust.Update {
		verb = "update"
	}
	msg := fmt.Sprintf("📦 %s is a signed gallery package. Here's everything %s will get. Nothing is written until you say so.\n%s"+
		"Reply **approve** to %s it, or **deny**.", file, who, preview, verb)
	t.askAs(t.char, msg, &store.Approval{UserID: t.user.ID, Character: t.char.ID, ChatRef: t.chatRef, Tool: internalPrefix + "import_sprig", Args: string(b)})
	e.logActivity(ctx, "info", "character", t.char.ID, t.user.ID, fmt.Sprintf("proposed a gallery %s of %s from %s (author key %s)", verb, who, file, g.Signer.KeyID), "")
	return fmt.Sprintf("PENDING: the user is being shown the author's key, every file %s will get and the scopes it asks for, and asked to approve. Tell them briefly to look it over; don't say it's done.", who), nil
}

// importGallery installs an approved gallery package, checked again.
func (e *Engine) importGallery(ctx context.Context, user *store.User, a sprigImportArgs, raw []byte) (string, error) {
	g, trust, err := e.readGallery(ctx, user, raw, a.ID, a.Name)
	if err != nil {
		return "", err
	}
	// Exactly what was approved: the same sprig, a new import or an
	// update as shown, and the same skills. Anything else needs a new look.
	if s := a.Gallery; s == nil || s.Sprig != g.Char.ID || s.Update != trust.Update || !slices.Equal(s.Skills, g.SkillIDs()) {
		return "", errors.New("what this package would do changed since it was shown (another import or update happened, or a skill id was taken or freed), so nothing was imported; ask again to see it as it is now")
	}
	res, err := g.Install(ctx, e.DB, e.Chars.Dir(), sprigpkg.GalleryInstallOptions{By: user.ID, Update: trust.Update})
	if err != nil {
		return "", err
	}
	e.pendingPkgs.Delete(user.ID)
	if err := e.Chars.Load(); err != nil {
		e.Log.Warn("reload sprigs after a gallery import", "err", err)
	}
	c := g.Char
	verb := "imported"
	if trust.Update {
		verb = "updated"
	} else {
		e.trailLevel(ctx, user, c.ID, store.AnyTool, "", store.LevelDraft, "a sprig from the gallery starts at draft")
	}
	e.addTrail(ctx, user.ID, c.ID, store.TrailGrant, fmt.Sprintf("%s %s from the gallery (author key %s); no scopes granted", verb, c.DisplayName(), g.Signer.KeyID), a.File, nil)
	e.logActivity(ctx, "info", "character", c.ID, user.ID, fmt.Sprintf("%s sprig %s from gallery package %s", verb, c.DisplayName(), a.File), res.Path)
	out := fmt.Sprintf("%s has been %s from the gallery (%s).", c.DisplayName(), verb, res.Path)
	if !trust.Update {
		out += " No scopes are granted and every tool is at draft."
		if len(g.Asks) > 0 {
			out += " It asks for: " + strings.Join(g.Asks, ", ") + "; the user can grant any of them."
		}
	}
	if len(res.Pending) > 0 {
		out += " Its skills wait for approval (manage_skill approve): " + strings.Join(res.Pending, ", ") + "."
	}
	return out, nil
}

// galleryShown is what a gallery import's approval prompt showed.
type galleryShown struct {
	Sprig  string   `json:"sprig"`
	Update bool     `json:"update"`
	Skills []string `json:"skills"`
}

// --- skills from a gallery package, waiting for approval ---

type approveSkillArgs struct {
	Sprig string `json:"sprig"`
	ID    string `json:"id"`
	Body  string `json:"body"` // the file as shown: a changed file isn't approved
}

// pendingSkills answers manage_skill's pending action.
func (e *Engine) pendingSkills(ctx context.Context, t *turn) (string, error) {
	if err := e.maySkill(ctx, t, t.char.ID, "see skills waiting for approval"); err != nil {
		return "", err
	}
	pend, err := sprigpkg.PendingSkills(e.Chars.Dir(), t.char.ID)
	if err != nil {
		return "", err
	}
	if len(pend) == 0 {
		return "You have no skills waiting for approval.", nil
	}
	var b strings.Builder
	b.WriteString("Skills from your gallery package, waiting for the user's approval (you can't follow them until then):\n")
	for _, k := range pend {
		fmt.Fprintf(&b, "- %s: %s\n", k.ID, k.Description)
	}
	return b.String(), nil
}

// proposeApproveSkill shows a pending skill in full and asks to make it
// the sprig's own.
func (e *Engine) proposeApproveSkill(ctx context.Context, t *turn, id string) (string, error) {
	if err := e.maySkill(ctx, t, t.char.ID, "approve a skill"); err != nil {
		return "", err
	}
	k, err := sprigpkg.PendingSkill(e.Chars.Dir(), t.char.ID, id)
	if err != nil {
		return "", err
	}
	f, err := sprigpkg.FormatSkillFile(*k)
	if err != nil {
		return "", err
	}
	b, _ := json.Marshal(approveSkillArgs{Sprig: t.char.ID, ID: id, Body: string(f)})
	msg := fmt.Sprintf("🧩 %s's skill **%s** came in a gallery package and is waiting for your OK. Approving it lets %s follow it:\n```markdown\n%s```\nReply **approve** or **deny**.",
		t.char.DisplayName(), id, t.char.DisplayName(), f)
	t.askAs(t.char, msg, &store.Approval{UserID: t.user.ID, Character: t.char.ID, ChatRef: t.chatRef, Tool: internalPrefix + "approve_skill", Args: string(b)})
	return "PENDING: the user is being shown the skill in full and asked to approve it. Don't follow it until they do.", nil
}

// approveSkill makes an approved pending skill the sprig's own.
func (e *Engine) approveSkill(ctx context.Context, user *store.User, args json.RawMessage) (string, error) {
	var a approveSkillArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return "", err
	}
	c, ok := e.Chars.Get(a.Sprig)
	if !ok {
		return "", fmt.Errorf("there's no sprig %q", a.Sprig)
	}
	if err := e.mayManage(ctx, user, c, "approve a skill"); err != nil {
		return "", err
	}
	k, err := sprigpkg.PendingSkill(e.Chars.Dir(), a.Sprig, a.ID)
	if err != nil {
		return "", err
	}
	if f, err := sprigpkg.FormatSkillFile(*k); err != nil || string(f) != a.Body {
		return "", sprigpkg.ErrPendingChanged
	}
	// The id must still be free for this sprig: a shared skill or another
	// sprig's may have taken it while this waited.
	if cur, err := e.DB.SkillByID(ctx, a.ID, 0); err == nil && cur.SprigID != a.Sprig {
		return "", fmt.Errorf("a skill called %s is already in use here (the shared library or another sprig's), so %s's can't be approved under that id", a.ID, c.DisplayName())
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	// Approved only as shown, checked again under the sprig's lock.
	if _, err := sprigpkg.ApprovePendingSkill(e.Chars.Dir(), a.Sprig, a.ID, []byte(a.Body)); err != nil {
		return "", err
	}
	if err := e.Chars.Load(); err != nil {
		e.Log.Warn("reload sprigs after approving a skill", "err", err)
	}
	for _, p := range e.SyncSprigSkills(ctx) {
		if filepath.Base(p.Path) == a.ID+".md" && filepath.Base(filepath.Dir(filepath.Dir(p.Path))) == a.Sprig {
			if rerr := sprigpkg.RestorePendingSkill(e.Chars.Dir(), a.Sprig, a.ID); rerr != nil {
				return "", fmt.Errorf("%s's skill %s couldn't be made its own (%s), and putting it back to waiting failed too: %w", c.DisplayName(), a.ID, p.Problem, rerr)
			}
			return "", fmt.Errorf("%s's skill %s couldn't be made its own: %s. It's still waiting for approval", c.DisplayName(), a.ID, p.Problem)
		}
	}
	e.addTrail(ctx, user.ID, c.ID, store.TrailSkill, fmt.Sprintf("skill %s (from a gallery package) approved for %s", a.ID, c.DisplayName()), "", nil)
	return fmt.Sprintf("%s's skill %s is approved: it's %s's own now.", c.DisplayName(), a.ID, c.DisplayName()), nil
}
