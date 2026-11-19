// Package setup is first-run configuration without a chat: `thicket init`
// asks for (or reads from the environment) the model, chat tokens,
// timezone, spend caps and owner, checks each against the real service,
// and stores it. Keys and tokens go into the encrypted secret store, the
// rest into settings; nothing is written to a plain config file.
package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/theshield2594/thicket/internal/config"
	"github.com/theshield2594/thicket/internal/llm"
	"github.com/theshield2594/thicket/internal/secrets"
	"github.com/theshield2594/thicket/internal/settings"
	"github.com/theshield2594/thicket/internal/store"
)

type Setup struct {
	DB       *store.Store
	Secrets  *secrets.Store
	Settings *settings.Settings
	// Verify checks keys, tokens and servers against the real services.
	// Off (--no-verify), values are stored as given.
	Verify bool
	HTTP   *http.Client
	// API bases, replaced in tests.
	OpenRouterAPI, DiscordAPI, TelegramAPI string
}

func (s *Setup) client() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return &http.Client{Timeout: 15 * time.Second}
}

func or(v, def string) string {
	if v != "" {
		return v
	}
	return def
}

// get fetches a URL and returns the body of a 2xx answer. Errors never
// quote the URL: Telegram's carries the token.
func (s *Setup) get(ctx context.Context, url string, header map[string]string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, errors.New("that address doesn't parse")
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := s.client().Do(req)
	if err != nil {
		return nil, errors.New("couldn't reach it (is this machine online?)")
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("it answered HTTP %d", resp.StatusCode)
	}
	return body, nil
}

// OpenRouterKey checks the key and stores it, and points models back at
// OpenRouter if a local server was set before.
func (s *Setup) OpenRouterKey(ctx context.Context, key string) (string, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return "", errors.New("the key is empty")
	}
	if s.Verify {
		if _, err := s.get(ctx, or(s.OpenRouterAPI, config.DefaultModelURL)+"/key", map[string]string{"Authorization": "Bearer " + key}); err != nil {
			return "", fmt.Errorf("OpenRouter didn't accept that key: %v", err)
		}
	}
	if err := s.Secrets.Put(ctx, secrets.OpenRouterKey, key); err != nil {
		return "", err
	}
	if !llm.IsOpenRouter(s.Settings.String(ctx, "model_url")) {
		if err := s.Settings.Set(ctx, "model_url", config.DefaultModelURL); err != nil {
			return "", err
		}
		// The local server's model id means nothing to OpenRouter.
		if err := s.Settings.Set(ctx, "model", ""); err != nil {
			return "", err
		}
	}
	return "OpenRouter key stored encrypted" + verified(s.Verify, "accepted by OpenRouter"), nil
}

// LocalModels lists what an OpenAI-compatible server (Ollama, LM Studio,
// llama.cpp, vLLM) serves. base ends in /v1 for most of them.
func (s *Setup) LocalModels(ctx context.Context, base, key string) ([]string, error) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		return nil, errors.New("the address needs to start with http:// or https://, like http://192.168.1.20:11434/v1")
	}
	models, err := llm.ListModels(ctx, base, strings.TrimSpace(key))
	if err != nil {
		return nil, fmt.Errorf("no model server answered at %s/models: %v", base, err)
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("the server at %s has no models yet (with Ollama: ollama pull llama3.1)", base)
	}
	return models, nil
}

// LocalModel points Thicket at a local server and one of its models.
func (s *Setup) LocalModel(ctx context.Context, base, model, key string) (string, error) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	model, key = strings.TrimSpace(model), strings.TrimSpace(key)
	if s.Verify {
		models, err := s.LocalModels(ctx, base, key)
		if err != nil {
			return "", err
		}
		switch {
		case model == "":
			model = models[0]
		case !contains(models, model):
			return "", fmt.Errorf("the server doesn't have a model called %q; it has: %s", model, strings.Join(models, ", "))
		}
	}
	if model == "" {
		return "", errors.New("a local server needs a model name (THICKET_MODEL), such as llama3.1")
	}
	if err := s.Settings.Set(ctx, "model_url", base); err != nil {
		return "", err
	}
	// One local model does every job unless told otherwise.
	for _, name := range []string{"model", "fallback_model", "cheap_model"} {
		if name != "model" && s.Settings.IsSet(ctx, name) {
			continue
		}
		if err := s.Settings.Set(ctx, name, model); err != nil {
			return "", err
		}
	}
	if key != "" {
		if err := s.Secrets.Put(ctx, secrets.ModelAPIKey, key); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("models run on %s, using %s", base, model) + verified(s.Verify, "the server answered"), nil
}

// DiscordToken checks the bot token and stores it.
func (s *Setup) DiscordToken(ctx context.Context, token string) (string, error) {
	token = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(token), "Bot "))
	if token == "" {
		return "", errors.New("the token is empty")
	}
	name := ""
	if s.Verify {
		body, err := s.get(ctx, or(s.DiscordAPI, "https://discord.com/api/v10")+"/users/@me", map[string]string{"Authorization": "Bot " + token})
		if err != nil {
			return "", fmt.Errorf("Discord didn't accept that token: %v", err)
		}
		var me struct {
			Username string `json:"username"`
		}
		_ = json.Unmarshal(body, &me)
		name = me.Username
	}
	if err := s.Secrets.Put(ctx, secrets.DiscordToken, token); err != nil {
		return "", err
	}
	return "Discord token stored encrypted" + verified(s.Verify, "bot "+or(name, "(unnamed)")), nil
}

// TelegramToken checks the bot token and stores it.
func (s *Setup) TelegramToken(ctx context.Context, token string) (string, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return "", errors.New("the token is empty")
	}
	name := ""
	if s.Verify {
		body, err := s.get(ctx, or(s.TelegramAPI, "https://api.telegram.org")+"/bot"+token+"/getMe", nil)
		if err != nil {
			return "", fmt.Errorf("Telegram didn't accept that token: %v", err)
		}
		var me struct {
			OK     bool `json:"ok"`
			Result struct {
				Username string `json:"username"`
			} `json:"result"`
		}
		if json.Unmarshal(body, &me) != nil || !me.OK {
			return "", errors.New("Telegram gave an answer that isn't a bot's")
		}
		name = me.Result.Username
	}
	if err := s.Secrets.Put(ctx, secrets.TelegramToken, token); err != nil {
		return "", err
	}
	return "Telegram token stored encrypted" + verified(s.Verify, "bot @"+name), nil
}

// Timezone sets the household's timezone: the one new people start with,
// and the owner's if there is one without a timezone yet.
func (s *Setup) Timezone(ctx context.Context, tz string) (string, error) {
	tz = strings.TrimSpace(tz)
	if err := s.Settings.Set(ctx, "timezone", tz); err != nil {
		return "", err
	}
	users, err := s.DB.ListUsers(ctx)
	if err != nil {
		return "", err
	}
	for _, u := range users {
		if u.Role == store.RoleOwner && u.Timezone == "" {
			if err := s.DB.UpdateUserProfile(ctx, u.ID, "", tz); err != nil {
				return "", err
			}
		}
	}
	return "timezone " + tz + " (the time there is " + nowIn(tz) + ")", nil
}

func nowIn(tz string) string {
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return "unknown"
	}
	return time.Now().In(loc).Format("15:04")
}

// Budgets sets the daily and monthly spend caps in USD ("" leaves one as
// it is; 0 means no cap).
func (s *Setup) Budgets(ctx context.Context, daily, monthly string) (string, error) {
	for name, v := range map[string]string{"budget.daily_usd": daily, "budget.monthly_usd": monthly} {
		if strings.TrimSpace(v) == "" {
			continue
		}
		if err := s.Settings.Set(ctx, name, v); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("spend caps: %s a day, %s a month", capText(s.Settings.Float(ctx, "budget.daily_usd")), capText(s.Settings.Float(ctx, "budget.monthly_usd"))), nil
}

func capText(v float64) string {
	if v == 0 {
		return "no cap"
	}
	return "$" + strconv.FormatFloat(v, 'f', 2, 64)
}

// Owner makes a chat account the owner, so the first message from it is
// recognised and nobody else can claim the install. who is "discord:ID"
// or "telegram:ID". An install that already has an owner keeps them.
func (s *Setup) Owner(ctx context.Context, who string) (string, error) {
	app, id, ok := strings.Cut(strings.TrimSpace(who), ":")
	app, id = strings.ToLower(strings.TrimSpace(app)), strings.TrimSpace(id)
	if !ok || (app != "discord" && app != "telegram") || id == "" || strings.ContainsAny(id, " \t\n") {
		return "", errors.New("write the owner as discord:USER_ID or telegram:USER_ID (the numeric id, not the name)")
	}
	if _, err := strconv.ParseInt(id, 10, 64); err != nil {
		return "", fmt.Errorf("%q isn't a numeric user id (on Discord: Developer Mode, then right-click yourself, Copy User ID; on Telegram: message @userinfobot)", id)
	}
	if u, err := s.DB.UserByExternal(ctx, app, id); err == nil {
		if u.Role == store.RoleOwner {
			return fmt.Sprintf("%s:%s is already the owner", app, id), nil
		}
		return "", fmt.Errorf("%s:%s is already here as a %s; change roles from the dashboard", app, id, u.Role)
	}
	if has, err := s.DB.HasOwner(ctx); err != nil {
		return "", err
	} else if has {
		return "", errors.New("this Thicket already has an owner; they can link another chat app by asking Juni for a link code")
	}
	if _, err := s.DB.CreateUser(ctx, app, id, "", store.RoleOwner); err != nil {
		return "", err
	}
	return fmt.Sprintf("owner: %s:%s (Juni will greet them on their first message)", app, id), nil
}

// State is what's configured now, for showing before asking. Secrets are
// reported as present or not, never their values.
type State struct {
	OpenRouterKey, ModelAPIKey  bool
	DiscordToken, TelegramToken bool
	ModelURL, Model             string
	Local                       bool
	Timezone                    string
	Daily, Monthly              string
	Owner                       string // "discord:123", or "" if nobody yet
}

func (s *Setup) State(ctx context.Context) (State, error) {
	st := State{
		OpenRouterKey: s.Secrets.Has(ctx, secrets.OpenRouterKey),
		ModelAPIKey:   s.Secrets.Has(ctx, secrets.ModelAPIKey),
		DiscordToken:  s.Secrets.Has(ctx, secrets.DiscordToken),
		TelegramToken: s.Secrets.Has(ctx, secrets.TelegramToken),
		ModelURL:      s.Settings.String(ctx, "model_url"),
		Model:         s.Settings.String(ctx, "model"),
		Timezone:      s.Settings.String(ctx, "timezone"),
		Daily:         capText(s.Settings.Float(ctx, "budget.daily_usd")),
		Monthly:       capText(s.Settings.Float(ctx, "budget.monthly_usd")),
	}
	st.Local = !llm.IsOpenRouter(st.ModelURL)
	users, err := s.DB.ListUsers(ctx)
	if err != nil {
		return st, err
	}
	for _, u := range users {
		if u.Role == store.RoleOwner {
			st.Owner = u.Channel + ":" + u.ExternalID
			if u.Channel == "web" {
				st.Owner = "a web app login" + nameSuffix(u.Name)
			}
			break
		}
	}
	return st, nil
}

// ModelReady reports whether a model can be called with what's stored.
func (st State) ModelReady() bool { return st.Local || st.OpenRouterKey }

func nameSuffix(name string) string {
	if name == "" {
		return ""
	}
	return " (" + name + ")"
}

func verified(on bool, what string) string {
	if on {
		return " (" + what + ")"
	}
	return " (not checked)"
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
