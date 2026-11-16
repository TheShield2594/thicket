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
	DefaultModelURL      = "https://openrouter.ai/api/v1"
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

	TelegramToken       string
	TelegramOwnerID     string
	TelegramListenChats []string

	OpenRouterKey string
	// ModelURL is the OpenAI-compatible API models are called through:
	// OpenRouter by default, or a local server such as Ollama.
	ModelURL    string
	ModelAPIKey string // optional key for a self-hosted server

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
		ModelURL:              strings.TrimRight(env("THICKET_MODEL_URL", env("OPENROUTER_BASE_URL", DefaultModelURL)), "/"),
		ModelAPIKey:           env("THICKET_MODEL_API_KEY", ""),
		Model:                 env("THICKET_MODEL", DefaultModel),
		FallbackModel:         env("THICKET_FALLBACK_MODEL", DefaultFallbackModel),
		CheapModel:            env("THICKET_CHEAP_MODEL", DefaultCheapModel),
		SecretKey:             env("THICKET_SECRET_KEY", ""),
		DiscordListenChannels: list(env("THICKET_DISCORD_CHANNELS", "")),
		AllowedUsers:          list(env("THICKET_ALLOWED_USERS", "")),
		TelegramToken:         env("TELEGRAM_TOKEN", ""),
		TelegramOwnerID:       env("THICKET_OWNER_TELEGRAM_ID", ""),
		TelegramListenChats:   list(env("THICKET_TELEGRAM_CHATS", "")),
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

// Problem is one item in the first-run check.
type Problem struct {
	Fatal bool
	What  string
	Fix   string
}

// Check reports exactly what is missing. Fatal problems stop `serve`.
func (c *Config) Check() []Problem {
	var out []Problem
	if c.DiscordToken == "" && c.TelegramToken == "" {
		out = append(out, Problem{true, "no chat app is set up (DISCORD_TOKEN or TELEGRAM_TOKEN)",
			"For Discord, create a bot at https://discord.com/developers/applications, enable the Message Content intent, and set DISCORD_TOKEN. " +
				"For Telegram, message @BotFather, send /newbot, and set TELEGRAM_TOKEN. You can use both."})
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
	switch {
	case c.ModelURL != DefaultModelURL:
		out = append(out, Problem{false, "models run on " + c.ModelURL,
			"Juni checks the server and picks a model on the first DM (set THICKET_MODEL to choose one yourself)."})
	case c.OpenRouterKey == "":
		out = append(out, Problem{false, "OPENROUTER_API_KEY is not set",
			"That's fine: Juni will ask for it in a DM and store it encrypted, or for the address of a local model server (Ollama, LM Studio) instead."})
	}
	if c.SecretKey == "" {
		out = append(out, Problem{false, "THICKET_SECRET_KEY is not set",
			"A key will be generated at " + filepath.Join(c.DataDir, "secret.key") + ". Back it up, or set THICKET_SECRET_KEY to keep it outside the volume."})
	}
	if c.DiscordOwnerID == "" && c.TelegramOwnerID == "" {
		out = append(out, Problem{false, "no owner id is set (THICKET_OWNER_DISCORD_ID or THICKET_OWNER_TELEGRAM_ID)",
			"The first person to DM the bot becomes the owner."})
	}
	return out
}

// OwnerID is the configured owner's account id on a chat app, if any.
func (c *Config) OwnerID(channel string) string {
	switch channel {
	case "discord":
		return c.DiscordOwnerID
	case "telegram":
		return c.TelegramOwnerID
	}
	return ""
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
