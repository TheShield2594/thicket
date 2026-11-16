// Package settings resolves runtime settings: a value set in chat (stored in
// the database) wins over the environment default.
package settings

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/theshield2594/thicket/internal/config"
	"github.com/theshield2594/thicket/internal/store"
)

type Key struct {
	Name string
	Help string
	Kind string // string | float | bool | int
}

// Known lists the settings users may change in chat. Anything else is refused.
var Known = []Key{
	{"model", "primary chat model (OpenRouter model id)", "string"},
	{"fallback_model", "model used when the primary fails", "string"},
	{"cheap_model", "small model for memory review and personality checks", "string"},
	{"budget.daily_usd", "daily spend cap in USD (0 = no cap)", "float"},
	{"budget.monthly_usd", "monthly spend cap in USD (0 = no cap)", "float"},
	{"consistency_check", "run the personality check on replies (true/false)", "bool"},
	{"checkin_days", "days of quiet before a sprig checks in (0 = never)", "int"},
	{"history_tokens", "how much of the conversation each reply includes, in tokens (0 = automatic: 12000 on OpenRouter, 3000 on a local model); older parts are summarised", "int"},
	{"model_url", "where models run: OpenRouter's API, or a local OpenAI-compatible server such as http://192.168.1.20:11434/v1 (Ollama)", "url"},
	{"web_search", "how sprigs with the web scope search: auto, openrouter (OpenRouter's web search, about $0.02 a search), searxng (your own SearXNG at search_url), or off", "choice"},
	{"search_url", "address of your SearXNG instance, e.g. http://192.168.1.20:8888", "url"},
	{"document_chars", "how much of a PDF's text is kept for follow-up questions, in characters (0 = none, at most 100000); it's re-sent with every later reply in the conversation", "int"},
	{"pdf_engine", "how PDFs are read: auto, native, cloudflare-ai (free, text only) or mistral-ocr (scans, $2 per 1,000 pages)", "choice"},
}

// intMax caps "int" settings that have an upper limit.
var intMax = map[string]int{"document_chars": 100000}

// choices lists the allowed values of "choice" settings.
var choices = map[string][]string{
	"pdf_engine": {"auto", "native", "cloudflare-ai", "mistral-ocr"},
	"web_search": {"auto", "openrouter", "searxng", "off"},
}

func lookup(name string) (Key, bool) {
	if strings.HasPrefix(name, "budget.character.") && strings.HasSuffix(name, ".daily_usd") {
		return Key{name, "per-character daily cap", "float"}, true
	}
	for _, k := range Known {
		if k.Name == name {
			return k, true
		}
	}
	return Key{}, false
}

type Settings struct {
	db  *store.Store
	cfg *config.Config
	// CharacterExists validates per-character budget keys. Set by the app.
	CharacterExists func(id string) bool
}

func New(db *store.Store, cfg *config.Config) *Settings { return &Settings{db: db, cfg: cfg} }

func (s *Settings) defaults() map[string]string {
	return map[string]string{
		"model":              s.cfg.Model,
		"fallback_model":     s.cfg.FallbackModel,
		"cheap_model":        s.cfg.CheapModel,
		"budget.daily_usd":   strconv.FormatFloat(s.cfg.DailyBudgetUSD, 'f', -1, 64),
		"budget.monthly_usd": strconv.FormatFloat(s.cfg.MonthlyBudgetUSD, 'f', -1, 64),
		"consistency_check":  "true",
		"checkin_days":       "3",
		"pdf_engine":         "auto",
		"web_search":         "auto",
		"search_url":         s.cfg.SearchURL,
		"history_tokens":     "0",
		"document_chars":     "20000",
		"model_url":          s.cfg.ModelURL,
	}
}

func (s *Settings) String(ctx context.Context, name string) string {
	if v, ok, err := s.db.Setting(ctx, name); err == nil && ok {
		return v
	}
	return s.defaults()[name]
}

// IsSet reports whether a setting was changed in chat (not a default).
func (s *Settings) IsSet(ctx context.Context, name string) bool {
	_, ok, err := s.db.Setting(ctx, name)
	return err == nil && ok
}

func (s *Settings) Float(ctx context.Context, name string) float64 {
	f, _ := strconv.ParseFloat(s.String(ctx, name), 64)
	return f
}

func (s *Settings) Int(ctx context.Context, name string) int {
	n, _ := strconv.Atoi(s.String(ctx, name))
	return n
}

func (s *Settings) Bool(ctx context.Context, name string) bool {
	b, _ := strconv.ParseBool(s.String(ctx, name))
	return b
}

// Set validates and stores a setting. An empty value resets to the default.
func (s *Settings) Set(ctx context.Context, name, value string) error {
	k, ok := lookup(name)
	if !ok {
		return fmt.Errorf("unknown setting %q", name)
	}
	if strings.HasPrefix(name, "budget.character.") {
		// The cap is read back by character id, so only a real id works.
		id := strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(name, "budget.character."), ".daily_usd"))
		if s.CharacterExists == nil || !s.CharacterExists(id) {
			return fmt.Errorf("no sprig with id %q; use the character id (e.g. juniper), not the nickname", id)
		}
		name = "budget.character." + id + ".daily_usd"
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return s.db.DeleteSetting(ctx, name)
	}
	switch k.Kind {
	case "float":
		f, err := strconv.ParseFloat(strings.TrimPrefix(value, "$"), 64)
		if err != nil || f < 0 {
			return fmt.Errorf("%s needs a non-negative number", name)
		}
		value = strconv.FormatFloat(f, 'f', -1, 64)
	case "int":
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 {
			return fmt.Errorf("%s needs a non-negative whole number", name)
		}
		if lim, ok := intMax[name]; ok && n > lim {
			return fmt.Errorf("%s can be at most %d", name, lim)
		}
	case "bool":
		b, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("%s needs true or false", name)
		}
		value = strconv.FormatBool(b)
	case "url":
		u, err := url.Parse(value)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || strings.ContainsAny(value, " \n\t") {
			return fmt.Errorf("%s needs an http(s) address like http://192.168.1.20:11434/v1", name)
		}
		value = strings.TrimRight(value, "/")
	case "choice":
		value = strings.ToLower(value)
		if !slices.Contains(choices[name], value) {
			return fmt.Errorf("%s must be one of: %s", name, strings.Join(choices[name], ", "))
		}
	case "string":
		if len(value) > 120 || strings.ContainsAny(value, " \n\t") {
			return fmt.Errorf("%s doesn't look like a model id", name)
		}
	}
	return s.db.SetSetting(ctx, name, value)
}

// Describe returns all current settings as readable lines.
func (s *Settings) Describe(ctx context.Context) []string {
	var out []string
	for _, k := range Known {
		out = append(out, fmt.Sprintf("%s = %s  (%s)", k.Name, s.String(ctx, k.Name), k.Help))
	}
	sort.Strings(out)
	return out
}
