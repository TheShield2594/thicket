// Package settings resolves runtime settings: a value set in chat (stored in
// the database) wins over the environment default.
package settings

import (
	"context"
	"fmt"
	"net/url"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

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
	{"auto_route", "when a message names no sprig, the cheap model picks who answers by role instead of the last sprig always keeping it (true/false; one small model call per such message)", "bool"},
	{"consistency_check", "run the personality check on replies (true/false)", "bool"},
	{"checkin_days", "days of quiet before a sprig checks in (0 = never)", "int"},
	{"history_tokens", "how much of the conversation each reply includes, in tokens (0 = automatic: 12000 on OpenRouter, 3000 on a local model); older parts are summarised", "int"},
	{"model_url", "where models run: OpenRouter's API, or a local OpenAI-compatible server such as http://192.168.1.20:11434/v1 (Ollama)", "url"},
	{"web_search", "how sprigs with the web scope search: auto, openrouter (OpenRouter's web search, about $0.02 a search), searxng (your own SearXNG at search_url), or off", "choice"},
	{"search_url", "address of your SearXNG instance, e.g. http://192.168.1.20:8888", "url"},
	{"document_chars", "how much of a PDF's text is kept for follow-up questions, in characters (0 = none, at most 100000); it's re-sent with every later reply in the conversation", "int"},
	{"transcribe_model", "model that transcribes voice messages; it must accept audio (OpenRouter model id)", "string"},
	{"timezone", "the household's timezone, which new people start with (IANA name such as Europe/London; each person can have their own)", "timezone"},
	{"backup_dir", "folder that scheduled full backups are written to, e.g. /backups (empty = off; in Docker it must be a mounted folder)", "path"},
	{"backup_schedule", "when scheduled backups run, as a cron line in the server's timezone (default 0 3 * * * = 03:00 every day)", "cron"},
	{"backup_keep", "how many scheduled backups to keep (the oldest are removed)", "int"},
	{"pdf_engine", "how PDFs are read: auto, native, cloudflare-ai (free, text only) or mistral-ocr (scans, $2 per 1,000 pages)", "choice"},
	{"limits.tool_calls_per_minute", "most tool calls one sprig makes in a minute (0 = no limit); limits.sprig.<id>.tool_calls_per_minute sets one sprig's", "int"},
	{"limits.actions_per_hour", "most actions one sprig takes in an hour without asking (tools that change something, at act level; 0 = no limit); limits.sprig.<id>.actions_per_hour sets one sprig's", "int"},
	{"limits.integration_calls_per_minute", "most calls to one integration in a minute, from all sprigs together (0 = no limit); limits.integration.<name>.calls_per_minute sets one integration's", "int"},
}

// cronParser reads backup_schedule the way job schedules are read.
var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// BackupSchedule parses a backup_schedule value.
func BackupSchedule(spec string) (cron.Schedule, error) { return cronParser.Parse(spec) }

// intMax caps "int" settings that have an upper limit.
var intMax = map[string]int{"document_chars": 100000, "backup_keep": 365}

// choices lists the allowed values of "choice" settings.
var choices = map[string][]string{
	"pdf_engine": {"auto", "native", "cloudflare-ai", "mistral-ocr"},
	"web_search": {"auto", "openrouter", "searxng", "off"},
}

func lookup(name string) (Key, bool) {
	if strings.HasPrefix(name, "budget.character.") && strings.HasSuffix(name, ".daily_usd") {
		return Key{name, "per-character daily cap", "float"}, true
	}
	if strings.HasPrefix(name, "limits.sprig.") && (strings.HasSuffix(name, ".tool_calls_per_minute") || strings.HasSuffix(name, ".actions_per_hour")) ||
		strings.HasPrefix(name, "limits.integration.") && strings.HasSuffix(name, ".calls_per_minute") {
		return Key{name, "rate limit override (0 = no limit)", "int"}, true
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
		"model":                               s.cfg.Model,
		"fallback_model":                      s.cfg.FallbackModel,
		"cheap_model":                         s.cfg.CheapModel,
		"budget.daily_usd":                    strconv.FormatFloat(s.cfg.DailyBudgetUSD, 'f', -1, 64),
		"budget.monthly_usd":                  strconv.FormatFloat(s.cfg.MonthlyBudgetUSD, 'f', -1, 64),
		"consistency_check":                   "true",
		"auto_route":                          "false",
		"checkin_days":                        "3",
		"limits.tool_calls_per_minute":        "30",
		"limits.actions_per_hour":             "20",
		"limits.integration_calls_per_minute": "60",
		"pdf_engine":                          "auto",
		"transcribe_model":                    config.DefaultFallbackModel,
		"web_search":                          "auto",
		"search_url":                          s.cfg.SearchURL,
		"history_tokens":                      "0",
		"document_chars":                      "20000",
		"model_url":                           s.cfg.ModelURL,
		"timezone":                            "",
		"backup_dir":                          "",
		"backup_schedule":                     "0 3 * * *",
		"backup_keep":                         "7",
	}
}

func (s *Settings) String(ctx context.Context, name string) string {
	if v, ok, err := s.db.Setting(ctx, name); err == nil && ok {
		return v
	}
	return s.defaults()[name]
}

// Default is the value a setting has until someone changes it.
func (s *Settings) Default(name string) string { return s.defaults()[name] }

// Choices lists the allowed values of a "choice" setting.
func Choices(name string) []string { return choices[name] }

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
		if name == "backup_keep" && n < 1 {
			return fmt.Errorf("%s must be at least 1", name)
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
	case "timezone":
		if _, err := time.LoadLocation(value); err != nil || value == "Local" {
			return fmt.Errorf("%s needs an IANA timezone name such as Europe/London or America/Chicago", name)
		}
	case "path":
		if !filepath.IsAbs(value) || len(value) > 500 || strings.ContainsAny(value, "\n\t\x00") {
			return fmt.Errorf("%s needs a full folder path such as /backups", name)
		}
		value = filepath.Clean(value)
	case "cron":
		if _, err := cronParser.Parse(value); err != nil {
			return fmt.Errorf("%s needs a cron line such as \"0 3 * * *\" (minute hour day month weekday): %v", name, err)
		}
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
