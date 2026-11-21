package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestJuniSuggestsAGallerySprig(t *testing.T) {
	a, fc, fm, ctx := setup(t)
	onboard(t, a, fc, ctx)
	withGallery(t, a)

	a.Engine.HandleIncoming(ctx, dm(`I want someone to track my plants. call tool gallery_search {"query":"track my plants"}`))
	fc.take()
	r := lastToolResult(fm)
	if !strings.Contains(r, `<untrusted source="gallery_search">`) || !strings.Contains(r, "name: fern") || strings.Contains(r, "ledger") {
		t.Fatalf("search: %s", r)
	}
	fm.mu.Lock()
	sys := systemPromptOf(fm.reqs[len(fm.reqs)-1])
	fm.mu.Unlock()
	if !strings.Contains(sys, "## The sprig gallery") {
		t.Fatal("Juni's prompt doesn't mention the gallery")
	}

	a.Engine.HandleIncoming(ctx, dm(`the plant one please. call tool gallery_preview {"name":"fern"}`))
	var preview string
	for _, s := range fc.take() {
		if strings.Contains(s.text, "signed gallery package") {
			preview = s.text
		}
	}
	if !strings.Contains(preview, "fern/skills/repot.md.pending") || !strings.Contains(preview, "**approve**") {
		t.Fatalf("no preview was shown: %q", preview)
	}
	if _, err := os.Stat(filepath.Join(a.Cfg.CharactersDir, "fern.md")); err == nil {
		t.Fatal("installed before approval")
	}
	a.Engine.HandleIncoming(ctx, dm("approve"))
	fc.take()
	c, ok := a.Chars.Get("fern")
	if !ok || len(c.Tools) != 0 || c.Host {
		t.Fatalf("after approval: %+v %v", c, ok)
	}
}
