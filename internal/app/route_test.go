package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func routerCalls(fm *fakeModel) int {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	n := 0
	for _, r := range fm.reqs {
		if strings.HasPrefix(r.Messages[0].Content, "You pick which character") {
			n++
		}
	}
	return n
}

func TestAutoRoute(t *testing.T) {
	a, fc, fm, ctx := setup(t)
	onboard(t, a, fc, ctx)
	// Pip is shared; Fern belongs to someone else, so it is never picked.
	for id, f := range map[string]string{"pip": sprigFile("pip", "Pip", 0), "fern": sprigFile("fern", "Fern", 999)} {
		if err := os.WriteFile(filepath.Join(a.Cfg.CharactersDir, id+".md"), []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Chars.Load(); err != nil || len(a.Chars.Errors()) > 0 {
		t.Fatalf("characters: %v %v", err, a.Chars.Errors())
	}
	who := func(msg string) string {
		t.Helper()
		a.Engine.HandleIncoming(ctx, dm(msg))
		out := fc.take()
		if len(out) == 0 {
			t.Fatalf("no reply to %q", msg)
		}
		return out[0].persona
	}

	// Off by default: the last sprig keeps the conversation, no extra call.
	if got := who("what's on my calendar?"); got != "Juni" {
		t.Fatalf("routing off: %s answered", got)
	}
	if routerCalls(fm) != 0 {
		t.Fatal("router called while auto_route is off")
	}

	if err := a.Settings.Set(ctx, "auto_route", "true"); err != nil {
		t.Fatal(err)
	}
	if got := who("what's on my calendar?"); got != "Pip" {
		t.Fatalf("calendar question went to %s, want Pip", got)
	}
	if got := who("thanks!"); got != "Pip" {
		t.Fatalf("follow-up went to %s, want Pip to keep it", got)
	}
	// Someone else's sprig, or a failed call: the current sprig answers.
	if got := who("how's the garden?"); got != "Pip" {
		t.Fatalf("another person's sprig picked: %s", got)
	}
	if got := who("router down please"); got != "Pip" {
		t.Fatalf("failed routing moved the chat to %s", got)
	}
	fm.mu.Lock()
	for _, r := range fm.reqs {
		if strings.HasPrefix(r.Messages[0].Content, "You pick which character") && strings.Contains(r.Messages[1].Content, "id fern") {
			t.Error("the router was shown another person's sprig")
		}
	}
	fm.mu.Unlock()
	// Naming a sprig skips the router.
	before := routerCalls(fm)
	if got := who("Juni, are you there?"); got != "Juni" {
		t.Fatalf("named sprig: %s answered", got)
	}
	if routerCalls(fm) != before {
		t.Fatal("router called for a message that names a sprig")
	}
}
