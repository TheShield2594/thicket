package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPDFPluginOnlyWithFiles(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = nil
		json.Unmarshal(b, &got)
		w.Write([]byte(`{"model":"m","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer srv.Close()
	o := NewOpenRouter(srv.URL, func(context.Context) (string, error) { return "k", nil })
	ctx := context.Background()

	o.Chat(ctx, Request{Model: "m", PDFEngine: "mistral-ocr", Messages: []Message{{Role: "user", Content: "hi"}}})
	if _, ok := got["plugins"]; ok {
		t.Fatal("plugin sent without a file")
	}
	o.Chat(ctx, Request{Model: "m", PDFEngine: "mistral-ocr", Messages: []Message{{Role: "user", Content: "hi", Files: []File{{Name: "a.pdf", URL: "data:application/pdf;base64,JVBE"}}}}})
	b, _ := json.Marshal(got["plugins"])
	if string(b) != `[{"id":"file-parser","pdf":{"engine":"mistral-ocr"}}]` {
		t.Fatalf("plugins = %s", b)
	}
	o.Chat(ctx, Request{Model: "m", Messages: []Message{{Role: "user", Content: "hi", Files: []File{{Name: "a.pdf", URL: "data:application/pdf;base64,JVBE"}}}}})
	if _, ok := got["plugins"]; ok {
		t.Fatal("auto engine should send no plugin")
	}
}
