package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/sprigpkg"
	"github.com/theshield2594/thicket/internal/store"
)

func TestImportSprigFromChat(t *testing.T) {
	f := newTaintFixture(t)
	ctx := context.Background()
	// A package made on another install.
	other := t.TempDir()
	c := &character.Character{ID: "fern", Name: "Fern", Role: "Keeps the garden on track.",
		Personality: character.Personality{Traits: []string{"patient"}}, Voice: character.Voice{Style: []string{"short"}}, Tools: []string{"schedule", "admin"}}
	md, _ := character.Marshal(c)
	os.WriteFile(filepath.Join(other, "fern.md"), md, 0o644)
	p, err := sprigpkg.Export(sprigpkg.Source{CharsDir: other, ID: "fern", LooksSecret: func(string) bool { return false },
		Jobs: []store.Job{{Character: "fern", Name: "morning", Schedule: "0 7 * * *", Prompt: "Say what to water.", Kind: store.JobCron, Enabled: true}}})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := p.Bytes()

	tr := f.turn("here's Fern, can you add her?")
	tr.packages = map[string][]byte{"fern.sprig": raw}
	out, err := f.e.toolImportSprig(ctx, tr, json.RawMessage(`{"file":"fern.sprig"}`))
	if err != nil || !strings.HasPrefix(out, "PENDING") {
		t.Fatalf("%v %s", err, out)
	}
	if strings.Contains(out, "water") {
		t.Fatal("the model saw the package's contents")
	}
	if _, err := os.Stat(filepath.Join(f.e.Chars.Dir(), "fern.md")); err == nil {
		t.Fatal("written before approval")
	}
	if len(tr.afterReply) != 1 || tr.afterReply[0].approval == nil || !strings.Contains(tr.afterReply[0].text, "+++ b/characters/fern.md") ||
		!strings.Contains(tr.afterReply[0].text, "admin") {
		t.Fatalf("approval: %+v", tr.afterReply)
	}
	a := tr.afterReply[0].approval
	if a.Tool != internalPrefix+"import_sprig" {
		t.Fatalf("tool %s", a.Tool)
	}
	// Approved.
	res, err := f.e.internalAction(ctx, f.user, a)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := f.e.Chars.Get("fern")
	if !ok || strings.Join(got.Tools, ",") != "schedule" {
		t.Fatalf("imported: %+v %s", got, res)
	}
	if lv, _ := f.e.DB.ExactToolLevel(ctx, "fern", store.AnyTool); lv != store.LevelDraft {
		t.Fatalf("level %q", lv)
	}
	if jobs, _ := f.e.DB.CharacterJobs(ctx, "fern"); len(jobs) != 1 || jobs[0].UserID != f.user.ID {
		t.Fatalf("routines: %+v", jobs)
	}
	// Sent again: refused with a suggestion.
	if _, err := f.e.toolImportSprig(ctx, tr, json.RawMessage(`{"file":"fern.sprig"}`)); err == nil || !strings.Contains(err.Error(), "fern-2") {
		t.Fatalf("clash: %v", err)
	}
}
