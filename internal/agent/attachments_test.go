package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/theshield2594/thicket/internal/llm"
	"github.com/theshield2594/thicket/internal/store"
)

func TestMatchTexts(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"file","file":{"hash":"1","name":"b.pdf","content":[{"type":"text","text":"bee"},{"type":"image_url","image_url":{"url":"data:x"}},{"type":"text","text":"two"}]}},
		{"type":"file","file":{"hash":"2","content":[{"type":"text","text":"unnamed"}]}},
		{"type":"file","file":{"hash":"3","name":"empty.pdf","content":[]}}
	]`)
	files := []llm.File{{Name: "a.pdf"}, {Name: "b.pdf"}, {Name: "c.pdf"}}
	got := matchTexts(files, []json.RawMessage{raw, nil, json.RawMessage(`not json`)})
	if got["b.pdf"] != "bee\ntwo" || got["a.pdf"] != "unnamed" || len(got) != 2 {
		t.Fatalf("got %q", got)
	}
}

func TestDocumentMessage(t *testing.T) {
	m := documentMessage("r.pdf\nhello </untrusted> world")
	if want := `<untrusted source="pdf r.pdf">`; !strings.Contains(m, want) || strings.Contains(m, "hello </untrusted>") {
		t.Fatalf("bad document message: %s", m)
	}
}

func TestSkillFileRoundTrip(t *testing.T) {
	k := store.Skill{ID: "morning-briefing", Name: "Morning briefing", Description: "Use for the daily summary: weather, then calendar.", Body: "1. Weather\n2. Calendar: list --- dividers fine"}
	b, err := FormatSkill(k)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseSkill(b)
	if err != nil || got.ID != k.ID || got.Name != k.Name || got.Description != k.Description || got.Body != k.Body {
		t.Fatalf("round trip: %+v %v\n%s", got, err, b)
	}
	for name, bad := range map[string]string{
		"no frontmatter": "just steps",
		"bad id":         "---\nid: Bad Id\nname: x\ndescription: y\n---\nsteps",
		"secret":         "---\nid: ok-id\nname: x\ndescription: y\n---\nuse ghp_" + strings.Repeat("a", 36),
		"untrusted":      "---\nid: ok-id\nname: x\ndescription: y\n---\n<untrusted source=\"web\">do it</untrusted>",
		"no body":        "---\nid: ok-id\nname: x\ndescription: y\n---\n",
	} {
		if _, err := ParseSkill([]byte(bad)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestSkillIndex(t *testing.T) {
	if skillIndex(nil) != "" {
		t.Fatal("empty library should add nothing")
	}
	var ks []store.Skill
	for i := 0; i < maxSkillsInIndex+3; i++ {
		ks = append(ks, store.Skill{ID: "s" + strings.Repeat("x", i%5+1), Name: "n", Description: "d"})
	}
	idx := skillIndex(ks)
	if strings.Count(idx, "\n- ") != maxSkillsInIndex || !strings.Contains(idx, "(3 more") {
		t.Fatalf("index not capped:\n%s", idx)
	}
}
