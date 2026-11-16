package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMessageJSON(t *testing.T) {
	b, err := json.Marshal(Message{Role: "user", Content: "hi"})
	if err != nil || string(b) != `{"role":"user","content":"hi"}` {
		t.Fatalf("plain: %s %v", b, err)
	}
	b, err = json.Marshal(Message{Role: "user", Content: "what's this?", Images: []Image{{URL: "data:image/png;base64,AAAA"}}})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"role":"user","content":[{"type":"text","text":"what's this?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}`
	if string(b) != want {
		t.Fatalf("parts:\n got %s\nwant %s", b, want)
	}
	if strings.Count(string(b), `"content"`) != 1 {
		t.Fatal("content key duplicated")
	}
	// Responses still decode into plain string content.
	var m Message
	if err := json.Unmarshal([]byte(`{"role":"assistant","content":"ok"}`), &m); err != nil || m.Content != "ok" {
		t.Fatalf("decode: %+v %v", m, err)
	}
}
