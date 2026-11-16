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

	b, err = json.Marshal(Message{Role: "user", Content: "read", Files: []File{{Name: "a.pdf", URL: "data:application/pdf;base64,JVBE"}}})
	if err != nil {
		t.Fatal(err)
	}
	want = `{"role":"user","content":[{"type":"text","text":"read"},{"type":"file","file":{"filename":"a.pdf","file_data":"data:application/pdf;base64,JVBE"}}]}`
	if string(b) != want {
		t.Fatalf("file:\n got %s\nwant %s", b, want)
	}
	// Annotations round-trip from a response back into the next request.
	if err := json.Unmarshal([]byte(`{"role":"assistant","content":"","annotations":[{"type":"file","file":{"hash":"h"}}]}`), &m); err != nil {
		t.Fatal(err)
	}
	b, _ = json.Marshal(m)
	if !strings.Contains(string(b), `"annotations":[{"type":"file","file":{"hash":"h"}}]`) {
		t.Fatalf("annotations lost: %s", b)
	}
}
