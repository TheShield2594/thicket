// Package config reads Thicket's environment configuration.
//
// Environment variables only provide defaults. Anything a user changes in
// chat is stored in the settings table and wins over the environment.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Defaults for model selection. All of these can be changed in chat.
const (
	DefaultModel         = "anthropic/claude-sonnet-5.5"
	DefaultFallbackModel = "google/gemini-3.8-flash"
	DefaultCheapModel    = "anthropic/claude-haiku-5.5"
)

type Config struct {
	DataDir       string
	CharactersDir string
	HTTPAddr      string
	PublicURL     string
	LogLevel      string

	DiscordToken          string
	DiscordOwnerID        string
	DiscordListenChannels []string

	OpenRouterKey string
	OpenRouterURL string

	Model         string
	FallbackModel string
	CheapModel    string

	SecretKey string

	DailyBudgetUSD   float64
	MonthlyBudgetUSD float64

	IdleAfter        time.Duration
	MessageRetention time.Duration
	AllowedUsers     []string
}

func Load() (*Config, error) {
	c := &Config{
		DataDir:               env("THICKET_DATA_DIR", "./data"),
		HTTPAddr:              env("THICKET_HTTP_ADDR", ":8080"),
		PublicURL:             strings.TrimRight(env("THICKET_PUBLIC_URL", ""), "/"),
		LogLevel:              env("THICKET_LOG_LEVEL", "info"),
		DiscordToken:          env("DISCORD_TOKEN", ""),
		DiscordOwnerID:        env("THICKET_OWNER_DISCORD_ID", ""),
		OpenRouterKey:         env("OPENROUTER_API_KEY", ""),
		OpenRouterURL:         env("OPENROUTER_BASE_URL", "https://openrouter.ai/api/v1"),
		Model:                 env("THICKET_MODEL", DefaultModel),
		FallbackModel:         env("THICKET_FALLBACK_MODEL", DefaultFallbackModel),
		CheapModel:            env("THICKET_CHEAP_MODEL", DefaultCheapModel),
		SecretKey:             env("THICKET_SECRET_KEY", ""),
		DiscordListenChannels: list(env("THICKET_DISCORD_CHANNELS", "")),
		AllowedUsers:          list(env("THICKET_ALLOWED_USERS", "")),
	}
	c.CharactersDir = env("THICKET_CHARACTERS_DIR", filepath.Join(c.DataDir, "characters"))

	var err error
	if c.DailyBudgetUSD, err = envFloat("THICKET_DAILY_BUDGET_USD", 2); err != nil {
		return nil, err
	}
	if c.MonthlyBudgetUSD, err = envFloat("THICKET_MONTHLY_BUDGET_USD", 30); err != nil {
		return nil, err
	}
	if c.IdleAfter, err = envDuration("THICKET_IDLE_AFTER", 20*time.Minute); err != nil {
		return nil, err
	}
	if c.MessageRetention, err = envDuration("THICKET_MESSAGE_RETENTION", 30*24*time.Hour); err != nil {
		return nil, err
	}
	return c, nil
}

// WebBaseURL is where the web app is reached: THICKET_PUBLIC_URL, or the
// local HTTP address.
func (c *Config) WebBaseURL() string {
	if c.PublicURL != "" {
		return c.PublicURL
	}
	if strings.HasPrefix(c.HTTPAddr, ":") {
		return "http://localhost" + c.HTTPAddr
	}
	return "http://" + c.HTTPAddr
}

// Problem is one item in the first-run check.
type Problem struct {
	Fatal bool
	What  string
	Fix   string
}

// Check reports exactly what is missing. Fatal problems stop `serve`.
func (c *Config) Check() []Problem {
	var out []Problem
	if c.DiscordToken == "" {
		out = append(out, Problem{false, "DISCORD_TOKEN is not set, so Discord is off",
			"That's fine if you only want the web app (run `thicket web link` for a sign-in link). For Discord, create a bot at https://discord.com/developers/applications, enable the Message Content intent, and set DISCORD_TOKEN."})
	}
	if err := os.MkdirAll(c.DataDir, 0o700); err != nil {
		out = append(out, Problem{true, "data directory " + c.DataDir + " is not writable: " + err.Error(),
			"Mount a writable volume at THICKET_DATA_DIR (default /data in Docker)."})
	} else if f, err := os.CreateTemp(c.DataDir, ".writecheck"); err != nil {
		out = append(out, Problem{true, "data directory " + c.DataDir + " is not writable: " + err.Error(),
			"Fix the volume permissions; the container runs as UID 65532."})
	} else {
		f.Close()
		os.Remove(f.Name())
	}
	if c.OpenRouterKey == "" {
		out = append(out, Problem{false, "OPENROUTER_API_KEY is not set",
			"That's fine: Juni will ask for it in a Discord DM and store it encrypted."})
	}
	if c.SecretKey == "" {
		out = append(out, Problem{false, "THICKET_SECRET_KEY is not set",
			"A key will be generated at " + filepath.Join(c.DataDir, "secret.key") + ". Back it up, or set THICKET_SECRET_KEY to keep it outside the volume."})
	}
	if c.DiscordToken != "" && c.DiscordOwnerID == "" {
		out = append(out, Problem{false, "THICKET_OWNER_DISCORD_ID is not set",
			"The first person to DM the bot becomes the owner."})
	}
	return out
}

func env(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func list(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envFloat(k string, def float64) (float64, error) {
	v := env(k, "")
	if v == "" {
		return def, nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("%s must be a non-negative number, got %q", k, v)
	}
	return f, nil
}

func envDuration(k string, def time.Duration) (time.Duration, error) {
	v := env(k, "")
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration like 20m, got %q", k, v)
	}
	return d, nil
}
