// Package settings resolves runtime settings: a value set in chat (stored in
// the database) wins over the environment default.
package settings

import (
	"context"
	"fmt"
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
	}
}

func (s *Settings) String(ctx context.Context, name string) string {
	if v, ok, err := s.db.Setting(ctx, name); err == nil && ok {
		return v
	}
	return s.defaults()[name]
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
	case "bool":
		b, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("%s needs true or false", name)
		}
		value = strconv.FormatBool(b)
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
