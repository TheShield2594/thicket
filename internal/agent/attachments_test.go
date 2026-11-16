package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/theshield2594/thicket/internal/llm"
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
