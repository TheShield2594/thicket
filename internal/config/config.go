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
	// CommonsRef is the shared channel where sprigs hand work to each
	// other by @mention, as "<app>:<chat id>" (a bare id means Discord).
	CommonsRef string

	OpenRouterKey string
	// ModelURL is the OpenAI-compatible API models are called through:
	// OpenRouter by default, or a local server such as Ollama.
	ModelURL    string
	ModelAPIKey string // optional key for a self-hosted server
	SearchURL   string // a SearXNG instance for web search (optional)
	// SandboxURL is the thicket-sandbox container's MCP address; when set,
	// Thicket connects it as the "sandbox" integration (sprigs' computer).
	SandboxURL string
	// SandboxTokenFile is where the token shared with the sandbox lives (a
	// small volume both containers mount).
	SandboxTokenFile string
	// WebProxy sends fetch_page through HTTPS_PROXY/HTTP_PROXY (see
	// docs/web.md for the trade-off).
	WebProxy bool

	Model         string
	FallbackModel string
	CheapModel    string

	SecretKey string
	// SecretKeyFile names a file holding the master key (Docker secrets).
	SecretKeyFile string

	DailyBudgetUSD   float64
	MonthlyBudgetUSD float64

	// OpenID Connect sign-in. The issuer, client id and secret are
	// stored encrypted on start (like the chat tokens); the rest are plain
	// settings.
	OIDCIssuer, OIDCClientID, OIDCClientSecret string
	OIDCName, OIDCGroupsClaim                  string
	OIDCAdminGroup, OIDCMemberGroup            string
	OIDCScopes                                 []string

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
		SearchURL:             strings.TrimRight(env("THICKET_SEARCH_URL", ""), "/"),
		SandboxURL:            strings.TrimRight(env("THICKET_SANDBOX_URL", ""), "/"),
		SandboxTokenFile:      env("THICKET_SANDBOX_TOKEN_FILE", ""),
		Model:                 env("THICKET_MODEL", DefaultModel),
		FallbackModel:         env("THICKET_FALLBACK_MODEL", DefaultFallbackModel),
		CheapModel:            env("THICKET_CHEAP_MODEL", DefaultCheapModel),
		SecretKey:             env("THICKET_SECRET_KEY", ""),
		SecretKeyFile:         env("THICKET_SECRET_KEY_FILE", ""),
		DiscordListenChannels: list(env("THICKET_DISCORD_CHANNELS", "")),
		AllowedUsers:          list(env("THICKET_ALLOWED_USERS", "")),
		TelegramToken:         env("TELEGRAM_TOKEN", ""),
		TelegramOwnerID:       env("THICKET_OWNER_TELEGRAM_ID", ""),
		TelegramListenChats:   list(env("THICKET_TELEGRAM_CHATS", "")),
		CommonsRef:            commonsRef(env("THICKET_COMMONS_CHANNEL", "")),
		OIDCIssuer:            strings.TrimRight(env("THICKET_OIDC_ISSUER", ""), "/"),
		OIDCClientID:          env("THICKET_OIDC_CLIENT_ID", ""),
		OIDCClientSecret:      env("THICKET_OIDC_CLIENT_SECRET", ""),
		OIDCName:              env("THICKET_OIDC_NAME", ""),
		OIDCGroupsClaim:       env("THICKET_OIDC_GROUPS_CLAIM", "groups"),
		OIDCAdminGroup:        env("THICKET_OIDC_ADMIN_GROUP", ""),
		OIDCMemberGroup:       env("THICKET_OIDC_MEMBER_GROUP", ""),
		OIDCScopes:            strings.Fields(strings.ReplaceAll(env("THICKET_OIDC_SCOPES", ""), ",", " ")),
	}
	switch strings.ToLower(env("THICKET_WEB_PROXY", "")) {
	case "", "0", "false", "no", "off":
	case "1", "true", "yes", "on":
		c.WebProxy = true
	default:
		return nil, fmt.Errorf("THICKET_WEB_PROXY must be 1 or 0, not %q", os.Getenv("THICKET_WEB_PROXY"))
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
	c.CommonsListen()
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
	// The web app always runs; Discord and Telegram are optional.
	if c.DiscordToken == "" {
		out = append(out, Problem{false, "DISCORD_TOKEN is not set, so Discord is off (unless its token is stored: see `thicket secrets list`)",
			"That's fine if you use the web app or Telegram (run `thicket web link` for a web sign-in link). For Discord, create a bot at https://discord.com/developers/applications, enable the Message Content intent, and store the token with `thicket secrets set discord_token` (or set DISCORD_TOKEN)."})
	}
	if c.TelegramToken == "" {
		out = append(out, Problem{false, "TELEGRAM_TOKEN is not set, so Telegram is off (unless its token is stored: see `thicket secrets list`)",
			"For Telegram, message @BotFather, send /newbot, and store the token with `thicket secrets set telegram_token` (or set TELEGRAM_TOKEN; see docs/telegram.md)."})
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
	switch {
	case c.SecretKey != "" && c.SecretKeyFile != "":
		out = append(out, Problem{true, "both THICKET_SECRET_KEY and THICKET_SECRET_KEY_FILE are set",
			"Set only one: THICKET_SECRET_KEY_FILE for a Docker secret, or THICKET_SECRET_KEY."})
	case c.SecretKeyFile != "":
		if b, err := os.ReadFile(c.SecretKeyFile); err != nil || strings.TrimSpace(string(b)) == "" {
			msg := "it's empty"
			if err != nil {
				msg = err.Error()
			}
			out = append(out, Problem{true, "THICKET_SECRET_KEY_FILE can't be read: " + msg,
				"Point it at the mounted secret, e.g. /run/secrets/thicket_secret_key."})
		}
	case c.SecretKey == "":
		out = append(out, Problem{false, "THICKET_SECRET_KEY is not set",
			"A key will be generated at " + filepath.Join(c.DataDir, "secret.key") + ". Back it up, or keep it outside the volume with THICKET_SECRET_KEY_FILE (a Docker secret) or THICKET_SECRET_KEY."})
	}
	if (c.DiscordToken != "" || c.TelegramToken != "") && c.DiscordOwnerID == "" && c.TelegramOwnerID == "" {
		out = append(out, Problem{false, "no owner id is set (THICKET_OWNER_DISCORD_ID or THICKET_OWNER_TELEGRAM_ID)",
			"The first person to DM the bot becomes the owner."})
	}
	return out
}

// EnvSecrets lists the secrets this configuration got from the
// environment (values may be empty).
func (c *Config) EnvSecrets() []EnvSecret {
	return []EnvSecret{
		{"discord_token", "DISCORD_TOKEN", c.DiscordToken},
		{"telegram_token", "TELEGRAM_TOKEN", c.TelegramToken},
		{"openrouter_api_key", "OPENROUTER_API_KEY", c.OpenRouterKey},
		{"model_api_key", "THICKET_MODEL_API_KEY", c.ModelAPIKey},
		{"oidc_issuer", "THICKET_OIDC_ISSUER", c.OIDCIssuer},
		{"oidc_client_id", "THICKET_OIDC_CLIENT_ID", c.OIDCClientID},
		{"oidc_client_secret", "THICKET_OIDC_CLIENT_SECRET", c.OIDCClientSecret},
	}
}

// EnvSecret is a secret named in the environment.
type EnvSecret struct{ Name, Env, Value string }

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

// commonsRef normalises THICKET_COMMONS_CHANNEL to a chat reference, and
// makes sure Thicket listens there.
func commonsRef(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if !strings.Contains(v, ":") {
		v = "discord:" + v
	}
	return v
}

// CommonsListen adds the commons channel to the chats the adapters answer
// everything in.
func (c *Config) CommonsListen() {
	app, id, _ := strings.Cut(c.CommonsRef, ":")
	switch app {
	case "discord":
		c.DiscordListenChannels = append(c.DiscordListenChannels, id)
	case "telegram":
		c.TelegramListenChats = append(c.TelegramListenChats, id)
	}
}
