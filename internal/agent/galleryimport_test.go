package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/sprigpkg"
	"github.com/theshield2594/thicket/internal/store"
)

func TestGalleryImportFromChat(t *testing.T) {
	f := newTaintFixture(t)
	ctx := context.Background()
	other := t.TempDir()
	c := &character.Character{ID: "fern", Name: "Fern", Role: "Keeps the garden on track.",
		Personality: character.Personality{Traits: []string{"patient"}}, Voice: character.Voice{Style: []string{"short"}}, Tools: []string{"schedule", "web"}}
	md, _ := character.Marshal(c)
	os.WriteFile(filepath.Join(other, "fern.md"), md, 0o644)
	lib := []store.Skill{{ID: "seed-swap", Name: "Seed swap", Description: "Use when trading seeds", Body: "1. list spares\n2. offer them", SprigID: "fern"},
		{ID: "trip-prep", Name: "Trip prep", Description: "Use when a trip is coming up", Body: "1. pack", SprigID: "fern"}}
	g, err := sprigpkg.ExportGallery(sprigpkg.GallerySource{CharsDir: other, ID: "fern", Library: lib, LooksSecret: func(string) bool { return false }})
	if err != nil {
		t.Fatal(err)
	}
	seed, _, _ := sprigpkg.NewSigningKey()
	priv, _ := sprigpkg.ParseSigningKey(seed)
	raw, err := g.Bytes(priv)
	if err != nil {
		t.Fatal(err)
	}

	tr := f.turn("here's Fern from the gallery")
	tr.packages = map[string][]byte{"fern.sprig": raw}
	out, err := f.e.toolImportSprig(ctx, tr, json.RawMessage(`{"file":"fern.sprig"}`))
	if err != nil || !strings.HasPrefix(out, "PENDING") {
		t.Fatalf("%v %s", err, out)
	}
	msg := tr.afterReply[0].text
	for _, want := range []string{"signed gallery package", g.Signer.Fingerprint(), "New author", "1. list spares", "schedule, web", "none of which are granted"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("preview lacks %q:\n%s", want, msg)
		}
	}
	if _, err := os.Stat(filepath.Join(f.e.Chars.Dir(), "fern.md")); err == nil {
		t.Fatal("written before approval")
	}
	res, err := f.e.internalAction(ctx, f.user, tr.afterReply[0].approval)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := f.e.Chars.Get("fern")
	if !ok || len(got.Tools) != 0 || !strings.Contains(res, "seed-swap") {
		t.Fatalf("imported: %+v %s", got, res)
	}
	if lv, _ := f.e.DB.ExactToolLevel(ctx, "fern", store.AnyTool); lv != store.LevelDraft {
		t.Fatalf("level %q", lv)
	}
	// The same approval again would now be an update of Fern, which isn't
	// what the person was shown: refused.
	if _, err := f.e.internalAction(ctx, f.user, tr.afterReply[0].approval); err == nil || !strings.Contains(err.Error(), "changed since it was shown") {
		t.Fatalf("a shown import ran as an update: %v", err)
	}
	if _, err := f.e.DB.SkillByID(ctx, "seed-swap", 0); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("the skill is active before approval")
	}

	// Fern asks to approve her skill; the person sees it in full.
	f.char = got
	tr = f.turn("yes, Fern can use her seed swap skill")
	if out, err := f.e.toolManageSkill(ctx, tr, json.RawMessage(`{"id":"","action":"pending"}`)); err != nil || !strings.Contains(out, "seed-swap") {
		t.Fatalf("pending: %v %s", err, out)
	}
	if out, err := f.e.toolManageSkill(ctx, tr, json.RawMessage(`{"id":"seed-swap","action":"approve"}`)); err != nil || !strings.HasPrefix(out, "PENDING") {
		t.Fatalf("approve: %v %s", err, out)
	}
	a := tr.afterReply[0].approval
	if a.Tool != internalPrefix+"approve_skill" || !strings.Contains(tr.afterReply[0].text, "1. list spares") {
		t.Fatalf("approval: %+v", tr.afterReply[0])
	}
	// Approving one as shown makes it Fern's own.
	tr2 := f.turn("and trip prep")
	if out, err := f.e.toolManageSkill(ctx, tr2, json.RawMessage(`{"id":"trip-prep","action":"approve"}`)); err != nil || !strings.HasPrefix(out, "PENDING") {
		t.Fatalf("approve: %v %s", err, out)
	}
	if _, err := f.e.internalAction(ctx, f.user, tr2.afterReply[0].approval); err != nil {
		t.Fatal(err)
	}
	if k, err := f.e.DB.SkillByID(ctx, "trip-prep", 0); err != nil || k.SprigID != "fern" {
		t.Fatalf("approved skill: %+v %v", k, err)
	}
	// Another sprig took the id while this waited: refused, still pending.
	if _, err := f.e.DB.SaveSkill(ctx, store.Skill{ID: "seed-swap", Name: "Pip's", Description: "Use when Pip trades", Body: "1. trade", SprigID: "pip"}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.internalAction(ctx, f.user, a); err == nil || !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("approved over another sprig's skill: %v", err)
	}
	if _, err := sprigpkg.PendingSkill(f.e.Chars.Dir(), "fern", "seed-swap"); err != nil {
		t.Fatalf("no longer pending: %v", err)
	}
	// Pip's skill is deleted, but its id stays reserved: the store refuses
	// Fern's when it's mirrored, so the file goes back to waiting and the
	// approval fails rather than reporting success.
	if err := f.e.DB.DeleteSprigSkill(ctx, "seed-swap", "pip"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.internalAction(ctx, f.user, a); err == nil || !strings.Contains(err.Error(), "still waiting") {
		t.Fatalf("approval reported success though the skill can't be used: %v", err)
	}
	if _, err := sprigpkg.PendingSkill(f.e.Chars.Dir(), "fern", "seed-swap"); err != nil {
		t.Fatalf("not put back to waiting: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.e.Chars.Dir(), "fern", "skills", "seed-swap.md")); err == nil {
		t.Fatal("the rejected skill file was left active")
	}

	// A different author's package for Fern is refused.
	seed2, _, _ := sprigpkg.NewSigningKey()
	priv2, _ := sprigpkg.ParseSigningKey(seed2)
	raw2, _ := g.Bytes(priv2)
	tr = f.turn("here's a newer Fern")
	tr.packages = map[string][]byte{"fern2.sprig": raw2}
	if _, err := f.e.toolImportSprig(ctx, tr, json.RawMessage(`{"file":"fern2.sprig"}`)); err == nil || !strings.Contains(err.Error(), "different key") {
		t.Fatalf("other author: %v", err)
	}
}
