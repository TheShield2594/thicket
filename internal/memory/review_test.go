package memory

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/config"
	"github.com/theshield2594/thicket/internal/llm"
	"github.com/theshield2594/thicket/internal/settings"
	"github.com/theshield2594/thicket/internal/store"
)

// scripted answers every review with the same ops.
type scripted struct {
	reply string
	asked []string
}

func (s *scripted) Chat(_ context.Context, req llm.Request) (*llm.Response, error) {
	s.asked = append(s.asked, req.Messages[len(req.Messages)-1].Content)
	return &llm.Response{Message: llm.Message{Role: "assistant", Content: s.reply}}, nil
}

func sprigFile(id, extra string) string {
	return fmt.Sprintf("---\nid: %s\nname: %s\nrole: A sprig.\n%spersonality:\n  traits: [kind]\nvoice:\n  style: [warm]\n---\nBody.\n", id, strings.ToUpper(id[:1])+id[1:], extra)
}

type reviewEnv struct {
	db    *store.Store
	r     *Reviewer
	model *scripted
	adult *store.User
	kid   *store.User
}

func newReviewEnv(t *testing.T) *reviewEnv {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dir, "thicket.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	adult, err := db.CreateUser(ctx, "discord", "sam", "Sam", store.RoleOwner)
	if err != nil {
		t.Fatal(err)
	}
	kid, err := db.CreateUser(ctx, "discord", "max", "Max", store.RoleKid)
	if err != nil {
		t.Fatal(err)
	}
	chars := filepath.Join(dir, "characters")
	os.MkdirAll(chars, 0o755)
	files := map[string]string{
		"juni":    sprigFile("juni", ""),
		"ivy":     sprigFile("ivy", fmt.Sprintf("belongs_to: %d\n", adult.ID)),
		"bramble": sprigFile("bramble", fmt.Sprintf("household: kid\nkids: [%d]\nparents: [%d]\n", kid.ID, adult.ID)),
	}
	for id, body := range files {
		if err := os.WriteFile(filepath.Join(chars, id+".md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := character.NewRegistry(chars, nil, log)
	if err := reg.Load(); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"juni", "ivy", "bramble"} {
		if _, ok := reg.Get(id); !ok {
			t.Fatalf("sprig %s didn't load", id)
		}
	}
	m := &scripted{}
	return &reviewEnv{db: db, model: m, adult: adult, kid: kid,
		r: &Reviewer{DB: db, LLM: m, Settings: settings.New(db, &config.Config{}), Chars: reg, Log: log, Idle: time.Minute}}
}

// review runs one conversation between user and sprig through the
// reviewer, with the model proposing ops.
func (e *reviewEnv) review(t *testing.T, user *store.User, sprig, said, ops string) int64 {
	t.Helper()
	ctx := context.Background()
	conv, err := e.db.OpenConversation(ctx, user.ID, "discord:dm-"+sprig+fmt.Sprint(user.ID), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	e.db.AddMessage(ctx, conv.ID, "user", "", said)
	e.db.AddMessage(ctx, conv.ID, "assistant", sprig, "Noted!")
	e.model.reply = ops
	run, err := e.r.Review(ctx, *conv)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

const householdFact = `{"ops":[{"op":"add","scope":"family","kind":"fact","content":"The household is vegetarian now."},
	{"op":"add","scope":"shared","kind":"fact","content":"Bins go out on Thursday."}]}`

func TestReviewProposesFamilyMemoryFromFamilySprigChat(t *testing.T) {
	e := newReviewEnv(t)
	ctx := context.Background()
	run := e.review(t, e.adult, "juni", "we're vegetarian now", householdFact)
	if !strings.Contains(e.model.asked[0], "Family memory is allowed") {
		t.Fatalf("the review wasn't told family memory is allowed:\n%s", e.model.asked[0])
	}
	fam, _ := e.db.FamilyMemories(ctx, "")
	if len(fam) != 1 || fam[0].Content != "The household is vegetarian now." || fam[0].UserID != e.adult.ID || fam[0].Locked {
		t.Fatalf("family memory: %+v", fam)
	}
	mine, _ := e.db.AllMemories(ctx, e.adult.ID)
	if len(mine) != 1 || mine[0].Content != "Bins go out on Thursday." {
		t.Fatalf("private memory: %+v", mine)
	}

	// The same fact again is de-duplicated against family memory, not
	// the person's private memories.
	e.review(t, e.adult, "juni", "still vegetarian", `{"ops":[{"op":"add","scope":"family","kind":"fact","content":"The household is vegetarian now"}]}`)
	if fam, _ := e.db.FamilyMemories(ctx, ""); len(fam) != 1 {
		t.Fatalf("a duplicate family memory was added: %+v", fam)
	}

	// Rollback takes family memory back out too.
	if _, err := e.db.RollbackRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if fam, _ := e.db.FamilyMemories(ctx, ""); len(fam) != 0 {
		t.Fatalf("rollback left family memory: %+v", fam)
	}
}

func TestReviewKeepsSoloSprigFactsPrivate(t *testing.T) {
	e := newReviewEnv(t)
	ctx := context.Background()
	e.review(t, e.adult, "ivy", "we're vegetarian now", householdFact)
	if !strings.Contains(e.model.asked[0], "Family memory is not allowed") {
		t.Fatalf("a solo sprig's review was offered family memory:\n%s", e.model.asked[0])
	}
	if fam, _ := e.db.FamilyMemories(ctx, ""); len(fam) != 0 {
		t.Fatalf("a solo sprig chat wrote family memory: %+v", fam)
	}
	if mine, _ := e.db.AllMemories(ctx, e.adult.ID); len(mine) != 1 || mine[0].Content != "Bins go out on Thursday." {
		t.Fatalf("private memory: %+v", mine)
	}
	// The model ignoring the rule and retrying as shared stays private.
	e.review(t, e.adult, "ivy", "we're vegetarian now", `{"ops":[{"op":"add","scope":"shared","kind":"fact","content":"The household is vegetarian now."}]}`)
	if mine, _ := e.db.AllMemories(ctx, e.adult.ID); len(mine) != 2 {
		t.Fatalf("private memory: %+v", mine)
	}
}

func TestReviewNeverWritesFamilyMemoryFromKids(t *testing.T) {
	e := newReviewEnv(t)
	ctx := context.Background()
	e.review(t, e.kid, "bramble", "we're vegetarian now", householdFact)
	if fam, _ := e.db.FamilyMemories(ctx, ""); len(fam) != 0 {
		t.Fatalf("a kid's chat wrote family memory: %+v", fam)
	}
	// A kid talking to a family sprig (not possible through routing, but
	// not trusted either) doesn't write it.
	e.review(t, e.kid, "juni", "we're vegetarian now", householdFact)
	if fam, _ := e.db.FamilyMemories(ctx, ""); len(fam) != 0 {
		t.Fatalf("a kid's chat wrote family memory: %+v", fam)
	}
}

func TestFamilyPlanRules(t *testing.T) {
	fam := store.Memory{ID: 9, UserID: 3, Kind: "fact", Content: "The household is vegetarian.", Household: true, Locked: true}
	in := PlanInput{UserID: 7, Existing: append(existing(), fam), Now: now, Loc: time.UTC, Family: true}
	p := BuildPlan(in, []Op{
		{Op: "add", Scope: "family", Kind: "fact", Content: "The household is vegetarian!"},
		{Op: "update", ID: 9, Content: "The household eats fish."},
		{Op: "add", Scope: "family", Kind: "followup", Content: "Check the bins.", Due: "2026-11-20"},
		{Op: "add", Scope: "family", Kind: "fact", Content: "Has a dog named Biscuit."}, // private twin: a new family memory
	})
	if len(p.Changes) != 1 || !p.Changes[0].Memory.Household || p.Changes[0].Memory.Content != "Has a dog named Biscuit." {
		t.Fatalf("changes: %+v skipped %v", p.Changes, p.Skipped)
	}
	if len(p.Skipped) != 3 {
		t.Fatalf("skipped: %v", p.Skipped)
	}
	in.Family = false
	if p := BuildPlan(in, []Op{{Op: "add", Scope: "family", Kind: "fact", Content: "The bins go out Thursday."}}); len(p.Changes) != 0 {
		t.Fatalf("family add allowed without Family: %+v", p.Changes)
	}
}
