package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/theshield2594/thicket/internal/config"
	"github.com/theshield2594/thicket/internal/llm"
	"github.com/theshield2594/thicket/internal/secrets"
	"github.com/theshield2594/thicket/internal/store"
)

// The Providers list on the Settings screen: OpenRouter, each hosted
// provider in the catalog, and the owner's own server. Models run on one
// of them at a time, but a key can be kept for several, so switching back
// doesn't mean pasting the key again.

// Ids of the two entries that aren't in the llm.Providers catalog.
const (
	providerOpenRouter = "openrouter"
	providerLocal      = "local"
)

// Provider states, as the Settings screen shows them.
const (
	providerInUse = "in_use" // models run here now
	providerReady = "ready"  // a key is stored; one click switches to it
	providerNone  = "none"
)

// ProviderInfo is one row of the Providers list.
type ProviderInfo struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Kind         string `json:"kind"` // openrouter | hosted | local
	Status       string `json:"status"`
	KeysURL      string `json:"keys_url,omitempty"`
	Note         string `json:"note,omitempty"`
	Subscription bool   `json:"subscription,omitempty"`
	Model        string `json:"model,omitempty"`   // the main model, when in use
	Address      string `json:"address,omitempty"` // the owner's server, when in use
}

// providerMu makes a switch one step: the key check, the stored keys and
// the settings change together.
var providerMu sync.Mutex

// providerKeyName is the secret a catalog provider's key is kept under
// while another provider is in use. The key of the one in use is in
// secrets.ModelAPIKey, where the model client reads it.
func providerKeyName(id string) string { return "model_key_" + strings.ReplaceAll(id, "-", "_") }

// keepProviderKey saves the key of the provider in use under the
// provider's own name, before something else takes the model key slot.
func (e *Engine) keepProviderKey(ctx context.Context) error {
	p, ok := e.provider(ctx)
	if !ok {
		return nil
	}
	key, _ := e.Secrets.Get(ctx, secrets.ModelAPIKey)
	if key == "" {
		return nil
	}
	return e.Secrets.Put(ctx, providerKeyName(p.ID), key)
}

// providerKey is the stored key for a catalog provider, or "".
func (e *Engine) providerKey(ctx context.Context, p llm.Provider) string {
	if cur, ok := e.provider(ctx); ok && cur.ID == p.ID {
		if k, _ := e.Secrets.Get(ctx, secrets.ModelAPIKey); k != "" {
			return k
		}
	}
	k, _ := e.Secrets.Get(ctx, providerKeyName(p.ID))
	return k
}

// AdminProviders lists every place models can run and how each stands.
func (e *Engine) AdminProviders(ctx context.Context) any {
	url := e.Settings.String(ctx, "model_url")
	model := e.Settings.String(ctx, "model")
	ready := e.ModelReady(ctx)
	cur, onProvider := llm.ProviderFor(url)

	or := ProviderInfo{ID: providerOpenRouter, Name: "OpenRouter", Kind: "openrouter", Status: providerNone, KeysURL: "https://openrouter.ai/keys",
		Note: "One key for hundreds of models, and the only one that also reads scanned PDFs, transcribes voice and searches the web."}
	if e.Secrets.Has(ctx, secrets.OpenRouterKey) {
		or.Status = providerReady
		if llm.IsOpenRouter(url) {
			or.Status, or.Model = providerInUse, model
		}
	}
	out := []ProviderInfo{or}
	for _, p := range llm.Providers {
		in := ProviderInfo{ID: p.ID, Name: p.Name, Kind: "hosted", Status: providerNone, KeysURL: p.KeysURL, Note: p.Note, Subscription: p.Subscription}
		if e.providerKey(ctx, p) != "" {
			in.Status = providerReady
			if onProvider && cur.ID == p.ID {
				in.Status, in.Model = providerInUse, model
			}
		}
		out = append(out, in)
	}
	local := ProviderInfo{ID: providerLocal, Name: "Your own server", Kind: "local", Status: providerNone,
		Note: "Ollama, LM Studio, llama.cpp or anything else with an OpenAI-compatible API. Free, but it can't read PDFs, and small models may not manage tools."}
	if !llm.IsHosted(url) {
		local.Address = url
		if ready {
			local.Status, local.Model = providerInUse, model
		}
	}
	return append(out, local)
}

func (e *Engine) modelLister() func(ctx context.Context, base, key string) ([]string, error) {
	if e.ListModels != nil {
		return e.ListModels
	}
	return llm.ListModels
}

// setAll stores several settings, in the order given.
func (e *Engine) setAll(ctx context.Context, kv ...string) error {
	for i := 0; i+1 < len(kv); i += 2 {
		if err := e.Settings.Set(ctx, kv[i], kv[i+1]); err != nil {
			return err
		}
	}
	return nil
}

// AdminConnectProvider switches models to a provider. A key is checked
// with the provider before anything is stored; with no key, the one
// already stored for it is used. address is for the owner's own server.
func (e *Engine) AdminConnectProvider(ctx context.Context, by *store.User, id, key, address string) (any, error) {
	if !by.IsAdmin() {
		return nil, errors.New("only the owner or an admin can change where models run")
	}
	id, key = strings.ToLower(strings.TrimSpace(id)), strings.TrimSpace(key)
	if key != "" {
		e.Secrets.Remember(key) // never in a log or an error, even if it's refused
	}
	providerMu.Lock()
	defer providerMu.Unlock()
	var err error
	switch id {
	case providerOpenRouter:
		err = e.useOpenRouter(ctx, by, key)
	case providerLocal:
		err = e.useLocal(ctx, by, address)
	default:
		p, ok := llm.ProviderByID(id)
		if !ok {
			return nil, fmt.Errorf("Thicket doesn't know a provider called %q", id)
		}
		err = e.useProvider(ctx, by, p, key)
	}
	if err != nil {
		return nil, errors.New(e.Secrets.Redact(err.Error()))
	}
	return e.AdminProviders(ctx), nil
}

func (e *Engine) useOpenRouter(ctx context.Context, by *store.User, key string) error {
	if key == "" && !e.Secrets.Has(ctx, secrets.OpenRouterKey) {
		return errors.New("OpenRouter needs a key (make one at https://openrouter.ai/keys)")
	}
	if key != "" {
		if e.KeyCheck != nil {
			cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			err := e.KeyCheck(cctx, key)
			cancel()
			if err != nil {
				return fmt.Errorf("OpenRouter didn't accept that key (%s)", shortErr(err))
			}
		}
		if err := e.Secrets.Put(ctx, secrets.OpenRouterKey, key); err != nil {
			return err
		}
	}
	if llm.IsOpenRouter(e.Settings.String(ctx, "model_url")) {
		e.logActivity(ctx, "info", "system", "", by.ID, "OpenRouter key replaced", "")
		return nil
	}
	if err := e.keepProviderKey(ctx); err != nil {
		return err
	}
	// Another server's model ids mean nothing to OpenRouter. Go back to
	// the defaults, unless those were set for another server too.
	url, model, fallback, cheap := "", "", "", ""
	if !llm.IsOpenRouter(e.Cfg.ModelURL) {
		url, model, fallback, cheap = config.DefaultModelURL, config.DefaultModel, config.DefaultFallbackModel, config.DefaultCheapModel
	}
	if err := e.setAll(ctx, "model_url", url, "model", model, "fallback_model", fallback, "cheap_model", cheap); err != nil {
		return err
	}
	e.logActivity(ctx, "info", "system", "", by.ID, "connected to model provider OpenRouter", e.Settings.String(ctx, "model"))
	return nil
}

func (e *Engine) useProvider(ctx context.Context, by *store.User, p llm.Provider, key string) error {
	cur, onProvider := e.provider(ctx)
	active := onProvider && cur.ID == p.ID
	fresh := key != ""
	if !fresh {
		if key = e.providerKey(ctx, p); key == "" {
			return fmt.Errorf("%s needs an API key (make one at %s)", p.Name, p.KeysURL)
		}
		if active {
			return nil // already there
		}
	}
	ids, err := e.modelLister()(ctx, p.BaseURL, key)
	if err != nil {
		return fmt.Errorf("%s didn't accept that key (%s)", p.Name, shortErr(err))
	}
	model, cheap := p.PickModels(ids)
	if model == "" {
		return fmt.Errorf("%s accepted the key, but lists no chat models. Is the account or plan active?", p.Name)
	}
	if !active {
		if err := e.keepProviderKey(ctx); err != nil {
			return err
		}
	}
	if err := e.Secrets.Put(ctx, providerKeyName(p.ID), key); err != nil {
		return err
	}
	if err := e.Secrets.Put(ctx, secrets.ModelAPIKey, key); err != nil {
		return err
	}
	if active {
		// A new key for the provider in use: the chosen models stay.
		e.logActivity(ctx, "info", "system", "", by.ID, p.Name+" key replaced", "")
		return nil
	}
	if err := e.setAll(ctx, "model_url", p.BaseURL, "model", model, "cheap_model", cheap, "fallback_model", model); err != nil {
		return err
	}
	e.logActivity(ctx, "info", "system", "", by.ID, "connected to model provider "+p.Name, model)
	return nil
}

func (e *Engine) useLocal(ctx context.Context, by *store.User, address string) error {
	address = strings.TrimSpace(address)
	if address == "" {
		return errors.New("your own server needs an address, like http://192.168.1.20:11434")
	}
	if llm.IsHosted(address) {
		return errors.New("that address is a hosted provider's: pick it from the list instead")
	}
	cands := localCandidates(address)
	if len(cands) == 0 {
		return errors.New("that doesn't look like a server's address; try something like http://192.168.1.20:11434")
	}
	var tried []string
	for _, base := range cands {
		// No key goes along: the stored one belongs to a hosted provider.
		ids, err := e.modelLister()(ctx, base, "")
		if err != nil {
			tried = append(tried, fmt.Sprintf("%s (%s)", base, shortErr(err)))
			continue
		}
		models := llm.ChatModels(ids)
		if len(models) == 0 {
			return fmt.Errorf("the server at %s has no chat models yet (with Ollama: ollama pull qwen2.5)", base)
		}
		if err := e.keepProviderKey(ctx); err != nil {
			return err
		}
		if _, ok := e.provider(ctx); ok {
			// A hosted provider's key must not be sent to this server.
			if err := e.Secrets.Delete(ctx, secrets.ModelAPIKey); err != nil {
				return err
			}
		}
		model := models[0]
		if err := e.setAll(ctx, "model_url", base, "model", model, "cheap_model", model, "fallback_model", model); err != nil {
			return err
		}
		e.logActivity(ctx, "info", "system", "", by.ID, "connected to local model server "+base, model)
		return nil
	}
	return fmt.Errorf("no model server answered at %s. If Thicket runs in Docker, localhost is the container itself: use the machine's LAN address or http://host.docker.internal:11434", strings.Join(tried, "; "))
}

// AdminDisconnectProvider forgets a provider's key. If models run there,
// sprigs can't reply until another provider is chosen.
func (e *Engine) AdminDisconnectProvider(ctx context.Context, by *store.User, id string) (any, error) {
	if !by.IsAdmin() {
		return nil, errors.New("only the owner or an admin can change where models run")
	}
	id = strings.ToLower(strings.TrimSpace(id))
	providerMu.Lock()
	defer providerMu.Unlock()
	fromEnv := func(name, env string) error {
		if e.Secrets.Has(ctx, name) {
			return fmt.Errorf("that key comes from the environment (%s); remove it there and restart", env)
		}
		return nil
	}
	name := "OpenRouter"
	switch id {
	case providerOpenRouter:
		if err := e.Secrets.Delete(ctx, secrets.OpenRouterKey); err != nil {
			return nil, err
		}
		if err := fromEnv(secrets.OpenRouterKey, "OPENROUTER_API_KEY"); err != nil {
			return nil, err
		}
	case providerLocal:
		return nil, errors.New("your own server has no key to remove; set up another provider to move off it")
	default:
		p, ok := llm.ProviderByID(id)
		if !ok {
			return nil, fmt.Errorf("Thicket doesn't know a provider called %q", id)
		}
		name = p.Name
		if err := e.Secrets.Delete(ctx, providerKeyName(p.ID)); err != nil {
			return nil, err
		}
		if cur, ok := e.provider(ctx); ok && cur.ID == p.ID {
			if err := e.Secrets.Delete(ctx, secrets.ModelAPIKey); err != nil {
				return nil, err
			}
			if err := fromEnv(secrets.ModelAPIKey, "THICKET_MODEL_API_KEY"); err != nil {
				return nil, err
			}
		}
	}
	e.logActivity(ctx, "info", "system", "", by.ID, name+" key removed", "")
	return e.AdminProviders(ctx), nil
}

// AdminModels lists the chat models on the server in use, for picking
// one on the Settings screen.
func (e *Engine) AdminModels(ctx context.Context) ([]string, error) {
	base := e.Settings.String(ctx, "model_url")
	name := secrets.ModelAPIKey
	if llm.IsOpenRouter(base) {
		name = secrets.OpenRouterKey
	}
	key, _ := e.Secrets.Get(ctx, name)
	ids, err := e.modelLister()(ctx, base, key)
	if err != nil {
		return nil, errors.New(e.Secrets.Redact(shortErr(err)))
	}
	return llm.SortedCopy(llm.ChatModels(ids)), nil
}
