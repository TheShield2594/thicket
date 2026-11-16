package app

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/theshield2594/thicket/internal/channel"
)

// fakeOllama serves an OpenAI-compatible model list.
func fakeOllama(t *testing.T, models string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"object":"list","data":[` + models + `]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

const someModels = `{"id":"qwen2.5:14b"},{"id":"nomic-embed-text:latest"},{"id":"llama3.1:8b"}`

func TestLocalModelOnboarding(t *testing.T) {
	a, fc, fm, ctx := setup(t)
	e := a.Engine
	srv := fakeOllama(t, someModels)

	e.HandleIncoming(ctx, dm("hi"))
	if out := fc.take(); len(out) != 1 || !strings.Contains(out[0].text, "Ollama") || !strings.Contains(out[0].text, "openrouter.ai/keys") {
		t.Fatalf("greeting should offer both ways: %+v", out)
	}
	if h := a.health(ctx); h["model_key"].OK {
		t.Fatal("health says ready before any model")
	}

	// An address nothing answers on: a useful explanation, still not ready.
	e.HandleIncoming(ctx, dm("http://127.0.0.1:1"))
	if out := fc.take(); len(out) != 1 || !strings.Contains(out[0].text, "couldn't reach") || !strings.Contains(out[0].text, "host.docker.internal") {
		t.Fatalf("unreachable server: %+v", out)
	}

	e.HandleIncoming(ctx, dm("it's at "+srv.URL))
	out := fc.take()
	if len(out) < 2 || !strings.Contains(out[0].text, "Connected to "+srv.URL+"/v1") || !strings.Contains(out[0].text, "**qwen2.5:14b**") ||
		!strings.Contains(out[0].text, "llama3.1:8b") || strings.Contains(out[0].text, "embed") {
		t.Fatalf("connect: %+v", out)
	}
	if !strings.Contains(out[1].text, "Juni") { // onboarding carries on, now with a model
		t.Fatalf("onboarding didn't continue: %+v", out)
	}
	for k, want := range map[string]string{"model_url": srv.URL + "/v1", "model": "qwen2.5:14b", "cheap_model": "qwen2.5:14b", "fallback_model": "qwen2.5:14b"} {
		if got := a.Settings.String(ctx, k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if h := a.health(ctx); !h["model_key"].OK || !strings.Contains(h["model_key"].Detail, srv.URL) {
		t.Fatalf("health: %+v", h["model_key"])
	}

	// PDFs are declined politely: local servers can't read them.
	a.Engine.FetchAttachment = fakeFetch
	in := dm("summarise")
	in.Attachments = []channel.Attachment{{Name: "report.pdf", URL: "https://cdn.example/report.pdf", ContentType: "application/pdf", Size: 100}}
	e.HandleIncoming(ctx, in)
	fc.take()
	if got := allUserContent(lastChatReq(fm)); !strings.Contains(got, "PDFs need OpenRouter") {
		t.Fatalf("pdf note missing:\n%s", got)
	}
}

func TestLocalServerWithoutChatModels(t *testing.T) {
	a, fc, _, ctx := setup(t)
	srv := fakeOllama(t, `{"id":"nomic-embed-text:latest"}`)
	a.Engine.HandleIncoming(ctx, dm(srv.URL))
	if out := fc.take(); len(out) != 1 || !strings.Contains(out[0].text, "no chat models") || !strings.Contains(out[0].text, "ollama pull") {
		t.Fatalf("no chat models: %+v", out)
	}
	if a.Engine.ModelReady(ctx) {
		t.Fatal("ready without a model")
	}
	// Changing their mind: an OpenRouter key switches back cleanly.
	a.Engine.HandleIncoming(ctx, dm("http://127.0.0.1:1"))
	fc.take()
	a.Engine.HandleIncoming(ctx, dm("sk-or-v1-"+strings.Repeat("ab12", 16)))
	fc.take()
	if !a.Engine.ModelReady(ctx) || a.Settings.IsSet(ctx, "model_url") {
		t.Fatalf("OpenRouter key after a local attempt: ready=%v url=%q", a.Engine.ModelReady(ctx), a.Settings.String(ctx, "model_url"))
	}
}

func TestLocalServerFromEnvironment(t *testing.T) {
	srv := fakeOllama(t, `{"id":"llama3.1:8b"}`)
	os.Setenv("THICKET_MODEL_URL", srv.URL+"/v1")
	t.Cleanup(func() { os.Unsetenv("THICKET_MODEL_URL") })
	a, fc, _, ctx := setup(t)
	a.Engine.HandleIncoming(ctx, dm("hello"))
	out := fc.take()
	if len(out) < 2 || !strings.Contains(out[0].text, "**llama3.1:8b**") {
		t.Fatalf("env server should connect on the first DM: %+v", out)
	}
	if a.Settings.IsSet(ctx, "model_url") {
		t.Fatal("an address from the environment shouldn't be copied into chat settings")
	}
}

func TestModelWithoutToolsStillAnswers(t *testing.T) {
	a, fc, fm, ctx := setup(t)
	srv := fakeOllama(t, `{"id":"tiny:1b"}`)
	a.Engine.HandleIncoming(ctx, dm(srv.URL))
	fc.take()
	fm.mu.Lock()
	fm.rejectTools = true
	fm.mu.Unlock()
	a.Engine.HandleIncoming(ctx, dm("how are you?"))
	out := fc.take()
	var reply, notice bool
	for _, s := range out {
		reply = reply || strings.Contains(s.text, "Lovely to meet you")
		notice = notice || strings.Contains(s.text, "can't use tools")
	}
	if !reply || !notice {
		t.Fatalf("want a reply and a one-time notice: %+v", out)
	}
	a.Engine.HandleIncoming(ctx, dm("still there?"))
	for _, s := range fc.take() {
		if strings.Contains(s.text, "can't use tools") {
			t.Fatal("tool notice repeated")
		}
	}
}
