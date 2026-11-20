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
	if skillIndex(nil, "") != "" {
		t.Fatal("empty library should add nothing")
	}
	var ks []store.Skill
	for i := 0; i < maxSkillsInIndex+3; i++ {
		ks = append(ks, store.Skill{ID: "s" + strings.Repeat("x", i%5+1), Name: "n", Description: "d"})
	}
	idx := skillIndex(ks, "")
	if strings.Count(idx, "\n- ") != maxSkillsInIndex || !strings.Contains(idx, "(3 more") {
		t.Fatalf("index not capped:\n%s", idx)
	}
}

func TestSkillPasswords(t *testing.T) {
	ok := store.Skill{ID: "login-help", Name: "Login help", Description: "When they're locked out", Body: "x"}
	for _, body := range []string{
		"1. Reset the password policy.\n2. Check the password field.",
		"The password is stored in the vault; never paste it.",
		"Log in with the password from {{secret:router}}.",
		"Use password managers.",
	} {
		ok.Body = body
		if err := ValidateSkill(ok); err != nil {
			t.Errorf("ordinary text refused: %q: %v", body, err)
		}
	}
	for _, body := range []string{
		"Log in with password velvet heron canoe",
		"The password is hunter2.",
		"password: Tr0ub4dor",
		`Wifi password "correct horse"`,
	} {
		ok.Body = body
		if err := ValidateSkill(ok); err == nil || !strings.Contains(err.Error(), "passwords") {
			t.Errorf("password accepted: %q", body)
		}
	}
	// Names and notes are checked too: names go into every prompt.
	ok.Body = "1. x"
	ok.Name = "ghp_" + strings.Repeat("a", 36)
	if ValidateSkill(ok) == nil {
		t.Error("a key in the name was accepted")
	}
	ok.Name, ok.Note = "Login help", "the password is hunter2"
	if ValidateSkill(ok) == nil {
		t.Error("a password in the note was accepted")
	}
}

func TestUniquePDFNames(t *testing.T) {
	var taken []llm.File
	for _, want := range []string{"receipt.pdf", "receipt (2).pdf", "receipt (3).pdf"} {
		n := uniqueName("receipt.pdf", taken)
		if n != want {
			t.Fatalf("got %q want %q", n, want)
		}
		taken = append(taken, llm.File{Name: n})
	}
}
