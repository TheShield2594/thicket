package agent

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/theshield2594/thicket/internal/channel"
	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/config"
	"github.com/theshield2594/thicket/internal/llm"
	"github.com/theshield2594/thicket/internal/secrets"
	"github.com/theshield2594/thicket/internal/store"
)

// Models run on OpenRouter (needs a key), on a hosted provider from the
// catalog in llm.Providers (needs that provider's key), or on any
// OpenAI-compatible server the owner runs themselves: Ollama, LM Studio,
// llama.cpp, vLLM. Choosing one is part of first-run setup: the owner sends
// a key or a server's address, and Thicket checks it and picks a model.

// ModelReady reports whether Thicket can call a model.
func (e *Engine) ModelReady(ctx context.Context) bool {
	url := e.Settings.String(ctx, "model_url")
	if llm.IsOpenRouter(url) {
		return e.Secrets.Has(ctx, secrets.OpenRouterKey)
	}
	if _, ok := llm.ProviderFor(url); ok && !e.Secrets.Has(ctx, secrets.ModelAPIKey) {
		return false
	}
	// Other servers need a model chosen for them: OpenRouter's default
	// model ids mean nothing there.
	return e.Settings.IsSet(ctx, "model") || e.Cfg.Model != config.DefaultModel
}

// localModel reports whether models run on a server the owner runs
// (free, maybe slow, no hosted extras).
func (e *Engine) localModel(ctx context.Context) bool {
	return !llm.IsHosted(e.Settings.String(ctx, "model_url"))
}

// onOpenRouter reports whether models run on OpenRouter, which alone has
// PDF parsing, audio input and web search built in.
func (e *Engine) onOpenRouter(ctx context.Context) bool {
	return llm.IsOpenRouter(e.Settings.String(ctx, "model_url"))
}

// provider is the catalog provider models run on, if any.
func (e *Engine) provider(ctx context.Context) (llm.Provider, bool) {
	return llm.ProviderFor(e.Settings.String(ctx, "model_url"))
}

var (
	urlInTextRe  = regexp.MustCompile(`(?i)\bhttps?://[^\s<>"']+`)
	hostPortRe   = regexp.MustCompile(`^\s*([a-z0-9.-]+|\[[0-9a-f:]+\]):(\d{2,5})(/\S*)?\s*$`)
	ollamaRe     = regexp.MustCompile(`(?i)\bollama\b`)
	lmStudioRe   = regexp.MustCompile(`(?i)\blm ?studio\b`)
	llamaCppRe   = regexp.MustCompile(`(?i)\bllama[.-]?cpp\b|\bllama-server\b`)
	dockerHostIP = "host.docker.internal" // see extra_hosts in docker-compose.yml
)

// localCandidates turns what the owner sent ("http://10.0.0.5:11434",
// "10.0.0.5:1234", "ollama") into base URLs to try, in order.
func localCandidates(text string) []string {
	var out []string
	add := func(u string) {
		u = strings.TrimRight(u, "/.,)")
		for _, x := range out {
			if x == u {
				return
			}
		}
		out = append(out, u)
	}
	withV1 := func(raw string) {
		u, err := url.Parse(strings.TrimRight(raw, "/.,)"))
		if err != nil || u.Host == "" || llm.IsHosted(raw) {
			return
		}
		switch {
		case strings.HasSuffix(u.Path, "/v1"):
			add(u.String())
		case u.Path == "" || u.Path == "/":
			u.Path = "/v1"
			add(u.String())
			u.Path = ""
			add(u.String())
		default:
			add(u.String())
			u.Path = strings.TrimRight(u.Path, "/") + "/v1"
			add(u.String())
		}
	}
	if m := urlInTextRe.FindString(text); m != "" {
		withV1(m)
		return out
	}
	if m := hostPortRe.FindStringSubmatch(text); m != nil {
		withV1("http://" + strings.TrimSpace(text))
		return out
	}
	defaults := func(port string, names ...string) {
		for _, h := range append([]string{dockerHostIP}, append(names, "localhost")...) {
			add("http://" + h + ":" + port + "/v1")
		}
	}
	switch {
	case ollamaRe.MatchString(text):
		defaults("11434", "ollama")
	case lmStudioRe.MatchString(text):
		defaults("1234")
	case llamaCppRe.MatchString(text):
		defaults("8080", "llama")
	}
	return out
}

func (e *Engine) listModels(ctx context.Context, base string) ([]string, error) {
	key, _ := e.Secrets.Get(ctx, secrets.ModelAPIKey)
	if e.ListModels != nil {
		return e.ListModels(ctx, base, key)
	}
	return llm.ListModels(ctx, base, key)
}

// bootstrap handles messages before Thicket can call a model. It is
// scripted: there's no model to talk with yet.
func (e *Engine) bootstrap(ctx context.Context, in channel.Incoming, user *store.User, host *character.Character) error {
	if !user.IsAdmin() || !in.IsDM {
		return e.bootstrapGreeting(ctx, in, user, host)
	}
	// An address in this message wins: it may be the replacement for a
	// configured server that didn't answer.
	if cands := localCandidates(in.Text); len(cands) > 0 {
		return e.connectLocal(ctx, in, user, host, cands, true)
	}
	// A provider and key set in the environment: pick its models now.
	if p, ok := e.provider(ctx); ok {
		if key, _ := e.Secrets.Get(ctx, secrets.ModelAPIKey); key != "" {
			return e.connectProvider(ctx, in, user, host, p, key, false)
		}
		return e.bootstrapGreeting(ctx, in, user, host)
	}
	// A server set in the environment: connect to it straight away.
	if e.localModel(ctx) {
		return e.connectLocal(ctx, in, user, host, []string{e.Settings.String(ctx, "model_url")}, false)
	}
	return e.bootstrapGreeting(ctx, in, user, host)
}

// connectProvider checks a hosted provider's key by listing its models,
// picks the models, and switches Thicket to it.
// sent says the key came in this message (so the message is deleted),
// rather than from the environment.
func (e *Engine) connectProvider(ctx context.Context, in channel.Incoming, user *store.User, host *character.Character, p llm.Provider, key string, sent bool) error {
	if ch := e.channelFor(in.Channel); ch != nil {
		ch.Typing(ctx, in.ChatID)
	}
	if sent {
		e.scrub(ctx, in) // whatever happens next, the key leaves the chat
	}
	list := llm.ListModels
	if e.ListModels != nil {
		list = e.ListModels
	}
	ids, err := list(ctx, p.BaseURL, key)
	if err != nil {
		return e.send(ctx, in.ChatRef(), host, fmt.Sprintf("Hmm, %s didn't accept that key (%s). Could you double-check it (keys are at %s) and send it again?", p.Name, shortErr(err), p.KeysURL))
	}
	model, cheap := p.PickModels(ids)
	if model == "" {
		return e.send(ctx, in.ChatRef(), host, fmt.Sprintf("%s accepted the key, but lists no chat models I can use. Is the account or plan active?", p.Name))
	}
	wasReady := e.ModelReady(ctx)
	if sent {
		if err := e.Secrets.Put(ctx, secrets.ModelAPIKey, key); err != nil {
			return err
		}
	}
	set := map[string]string{"model_url": p.BaseURL, "model": model, "cheap_model": cheap, "fallback_model": model}
	for _, k := range []string{"model_url", "model", "cheap_model", "fallback_model"} {
		if err := e.Settings.Set(ctx, k, set[k]); err != nil {
			return e.send(ctx, in.ChatRef(), host, "The key works, but I couldn't save the setting ("+shortErr(err)+").")
		}
	}
	e.logActivity(ctx, "info", "system", "", user.ID, "connected to model provider "+p.Name, model)
	msg := fmt.Sprintf("Connected to %s. I'll think with **%s**", p.Name, model)
	if sent {
		msg = fmt.Sprintf("Got it, and it works! I've locked the key away encrypted. I'll think with **%s** on %s", model, p.Name)
	}
	if cheap != model {
		msg += fmt.Sprintf(", and use **%s** for quick background jobs", cheap)
	}
	msg += ". Say \"use <model>\" any time to change."
	if p.Note != "" {
		msg += "\n\n" + p.Note
	}
	if !p.Subscription {
		msg += "\n\n" + p.Name + " doesn't tell me what each call costs, so spend caps only count it if you tell me its prices: set `model_usd_per_mtok_in` and `model_usd_per_mtok_out` (dollars per million tokens)."
	}
	if err := e.send(ctx, in.ChatRef(), host, msg); err != nil {
		return err
	}
	if wasReady {
		return nil
	}
	return e.continueAfterEvent(ctx, in, user, host,
		fmt.Sprintf("Thicket is now running on %s (model %s); the key is stored safely, don't mention it. Continue onboarding from where things stand.", p.Name, model))
}

// connectLocal tries each candidate server, and on the first with a chat
// model, switches Thicket to it and carries on with onboarding.
func (e *Engine) connectLocal(ctx context.Context, in channel.Incoming, user *store.User, host *character.Character, cands []string, saveURL bool) error {
	if ch := e.channelFor(in.Channel); ch != nil {
		ch.Typing(ctx, in.ChatID)
	}
	var tried []string
	for _, base := range cands {
		ids, err := e.listModels(ctx, base)
		if err != nil {
			tried = append(tried, fmt.Sprintf("%s (%s)", base, shortErr(err)))
			continue
		}
		models := llm.ChatModels(ids)
		if len(models) == 0 {
			return e.send(ctx, in.ChatRef(), host, fmt.Sprintf("I found a model server at %s, but it has no chat models yet. "+
				"For Ollama, run `ollama pull qwen2.5` (or another model that supports tools) on that machine, then send me the address again.", base))
		}
		model := models[0]
		set := map[string]string{"model": model, "cheap_model": model, "fallback_model": model}
		if saveURL {
			set["model_url"] = base
		}
		for k, v := range set {
			if err := e.Settings.Set(ctx, k, v); err != nil {
				return e.send(ctx, in.ChatRef(), host, "I reached "+base+" but couldn't save the setting ("+shortErr(err)+").")
			}
		}
		e.logActivity(ctx, "info", "system", "", 0, "connected to local model server "+base, model)
		msg := fmt.Sprintf("Connected to %s. I'll think with **%s**.", base, model)
		if len(models) > 1 {
			others := llm.SortedCopy(models[1:])
			if len(others) > 8 {
				others = append(others[:8], "…")
			}
			msg += fmt.Sprintf(" Also there: %s. Say \"use <name>\" whenever you want a different one.", strings.Join(others, ", "))
		}
		msg += "\n\nA heads-up about local models: they can't read PDFs, and small ones may not be able to use tools (that's how I remember things and schedule jobs). If yours can't, I'll tell you."
		if err := e.send(ctx, in.ChatRef(), host, msg); err != nil {
			return err
		}
		return e.continueAfterEvent(ctx, in, user, host,
			fmt.Sprintf("Thicket is now running on the user's own model server (%s, model %s). Continue onboarding from where things stand.", base, model))
	}
	hint := "If Thicket runs in Docker, `localhost` there is the container itself: use your computer's LAN address (like http://192.168.1.20:11434), " +
		"or http://host.docker.internal:11434 with the compose file's extra_hosts line. For Ollama, also start it with OLLAMA_HOST=0.0.0.0 so it listens beyond localhost."
	return e.send(ctx, in.ChatRef(), host, "I couldn't reach a model server at "+strings.Join(tried, "; ")+".\n\n"+hint+
		"\n\nSend me the address to try again, or an OpenRouter key from https://openrouter.ai/keys instead (or another provider's key, with its name: \"groq gsk_…\").")
}
