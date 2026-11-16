package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// capture serves chat completions and records the last request.
type capture struct {
	body map[string]any
	auth string
}

func (c *capture) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.Write([]byte(`{"data":[{"id":"llama3.1:8b"},{"id":"nomic-embed-text"},{"id":"qwen2.5:14b"}]}`))
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.NotFound(w, r)
			return
		}
		b, _ := io.ReadAll(r.Body)
		c.body = nil
		json.Unmarshal(b, &c.body)
		c.auth = r.Header.Get("Authorization")
		w.Write([]byte(`{"model":"m","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// toServer sends every request to srv, whatever host it names, so a
// client configured for openrouter.ai can be tested offline.
type toServer struct{ srv *url.URL }

func (s toServer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.URL.Scheme, r.URL.Host = s.srv.Scheme, s.srv.Host
	r.URL.Path = strings.TrimPrefix(r.URL.Path, "/api")
	return http.DefaultTransport.RoundTrip(r)
}

var pdfMsg = []Message{{Role: "user", Content: "hi", Files: []File{{Name: "a.pdf", URL: "data:application/pdf;base64,JVBE"}}}}

func TestOpenRouterExtensions(t *testing.T) {
	c := &capture{}
	srv := c.server(t)
	u, _ := url.Parse(srv.URL)
	o := NewOpenRouter(DefaultURL, func(context.Context, string) (string, error) { return "k", nil })
	o.HTTP = &http.Client{Transport: toServer{u}}
	ctx := context.Background()

	o.Chat(ctx, Request{Model: "m", PDFEngine: "mistral-ocr", Messages: []Message{{Role: "user", Content: "hi"}}})
	if _, ok := c.body["plugins"]; ok {
		t.Fatal("plugin sent without a file")
	}
	if c.body["usage"] == nil || c.auth != "Bearer k" {
		t.Fatalf("OpenRouter request should ask for usage and send the key: %v %q", c.body, c.auth)
	}
	o.Chat(ctx, Request{Model: "m", PDFEngine: "mistral-ocr", Messages: pdfMsg})
	b, _ := json.Marshal(c.body["plugins"])
	if string(b) != `[{"id":"file-parser","pdf":{"engine":"mistral-ocr"}}]` {
		t.Fatalf("plugins = %s", b)
	}
	o.Chat(ctx, Request{Model: "m", Messages: pdfMsg})
	if _, ok := c.body["plugins"]; ok {
		t.Fatal("auto engine should send no plugin")
	}
	o.Chat(ctx, Request{Model: "m", PDFEngine: "cloudflare-ai", WebResults: 5, Messages: pdfMsg})
	b, _ = json.Marshal(c.body["plugins"])
	if string(b) != `[{"id":"file-parser","pdf":{"engine":"cloudflare-ai"}},{"id":"web","max_results":5}]` {
		t.Fatalf("both plugins = %s", b)
	}

	noKey := NewOpenRouter(DefaultURL, func(context.Context, string) (string, error) { return "", nil })
	if _, err := noKey.Chat(ctx, Request{Model: "m"}); err != ErrNoKey {
		t.Fatalf("OpenRouter without a key: %v", err)
	}
}

func TestLocalServer(t *testing.T) {
	c := &capture{}
	srv := c.server(t)
	o := NewOpenRouter(DefaultURL, func(context.Context, string) (string, error) { return "", nil })
	o.URL = func(context.Context) string { return srv.URL + "/v1" }
	resp, err := o.Chat(context.Background(), Request{Model: "llama3.1:8b", PDFEngine: "native", Messages: pdfMsg})
	if err != nil || resp.Message.Content != "ok" {
		t.Fatalf("local chat without a key: %v", err)
	}
	if _, ok := c.body["usage"]; ok {
		t.Fatal("OpenRouter's usage field sent to a local server")
	}
	if _, ok := c.body["plugins"]; ok {
		t.Fatal("OpenRouter's PDF plugin sent to a local server")
	}
	if c.auth != "" {
		t.Fatalf("empty key sent as %q", c.auth)
	}

	ids, err := ListModels(context.Background(), srv.URL+"/v1", "")
	if err != nil || len(ids) != 3 {
		t.Fatalf("models: %v %v", ids, err)
	}
	if got := ChatModels(ids); len(got) != 2 || got[0] != "llama3.1:8b" {
		t.Fatalf("chat models: %v", got)
	}
	if _, err := ListModels(context.Background(), srv.URL+"/nope", ""); err == nil {
		t.Fatal("404 model list accepted")
	}
}

func TestIsOpenRouter(t *testing.T) {
	for u, want := range map[string]bool{
		DefaultURL:                    true,
		"https://eu.openrouter.ai/v1": true,
		"http://localhost:11434/v1":   false,
		"https://openrouter.ai.evil":  false,
		"http://openrouter.ai@host/":  false,
	} {
		if IsOpenRouter(u) != want {
			t.Errorf("IsOpenRouter(%q) != %v", u, want)
		}
	}
	tools := &APIError{Status: 400, Message: "registry.ollama.ai/library/gemma:2b does not support tools"}
	if !ToolsRejected(tools) || ToolsRejected(&APIError{Status: 503, Message: "tools down"}) {
		t.Fatal("ToolsRejected")
	}
	if MediaRejected(tools) || MediaRejected(&APIError{Status: 400, Message: "This model's maximum context length is 8192 tokens"}) {
		t.Fatal("a tools or context error must not be taken for a media rejection (the retry would drop the image)")
	}
	if !MediaRejected(&APIError{Status: 404, Message: "No endpoints found that support image input"}) {
		t.Fatal("MediaRejected")
	}
}

// The key is chosen for the server the call actually goes to.
func TestKeyFollowsTheCallsServer(t *testing.T) {
	c := &capture{}
	srv := c.server(t)
	var asked string
	o := NewOpenRouter(DefaultURL, func(_ context.Context, base string) (string, error) {
		asked = base
		if IsOpenRouter(base) {
			return "openrouter-key", nil
		}
		return "", nil
	})
	o.URL = func(context.Context) string { return srv.URL + "/v1" }
	o.Chat(context.Background(), Request{Model: "m"})
	if asked != srv.URL+"/v1" || c.auth != "" {
		t.Fatalf("key looked up for %q, sent %q", asked, c.auth)
	}
}
