package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/theshield2594/thicket/internal/config"
	"github.com/theshield2594/thicket/internal/secrets"
	"github.com/theshield2594/thicket/internal/settings"
	"github.com/theshield2594/thicket/internal/setup"
)

// initCmd is first-run setup at a terminal (or from the environment with
// --from-env). It can be run again any time: it shows what's set, with
// secrets only as "stored", and changes just what's typed.
func initCmd(args []string, in io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fromEnv := fs.Bool("from-env", false, "don't ask: check and store what's in the environment (OPENROUTER_API_KEY, DISCORD_TOKEN, ...)")
	noVerify := fs.Bool("no-verify", false, "store keys and tokens without checking them against the services (for a machine that's offline)")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return errors.New("usage: thicket init [--from-env] [--no-verify]")
	}
	cfg := loadConfig()
	// Only what stops Thicket: the notes are about the very things init
	// is about to ask for.
	var fatal []config.Problem
	for _, p := range cfg.Check() {
		if p.Fatal {
			fatal = append(fatal, p)
		}
	}
	if printProblems(fatal) {
		return errors.New("fix the items marked MISSING first")
	}
	db, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	key, _, err := secrets.ResolveKey(cfg.SecretKey, cfg.SecretKeyFile, cfg.DataDir)
	if err != nil {
		return err
	}
	sec, err := secrets.New(db, key, secrets.NewRedactor())
	if err != nil {
		return err
	}
	s := &setup.Setup{DB: db, Secrets: sec, Settings: settings.New(db, cfg), Verify: !*noVerify}
	ctx := context.Background()
	if *fromEnv {
		err = initFromEnv(ctx, s, cfg, out)
	} else {
		err = initAsk(ctx, s, cfg, in, out)
	}
	if err != nil {
		return err
	}
	return initFinish(ctx, s, cfg, out)
}

// initFromEnv stores what the environment gives, checking each value. Any
// value that fails its check fails the command; the rest are still stored.
func initFromEnv(ctx context.Context, s *setup.Setup, cfg *config.Config, out io.Writer) error {
	type step struct {
		what string
		do   func() (string, error)
	}
	var steps []step
	if cfg.ModelURL != config.DefaultModelURL {
		model := ""
		if _, ok := os.LookupEnv("THICKET_MODEL"); ok {
			model = cfg.Model
		}
		steps = append(steps, step{"THICKET_MODEL_URL", func() (string, error) { return s.LocalModel(ctx, cfg.ModelURL, model, cfg.ModelAPIKey) }})
	} else if cfg.OpenRouterKey != "" {
		steps = append(steps, step{"OPENROUTER_API_KEY", func() (string, error) { return s.OpenRouterKey(ctx, cfg.OpenRouterKey) }})
	}
	if cfg.DiscordToken != "" {
		steps = append(steps, step{"DISCORD_TOKEN", func() (string, error) { return s.DiscordToken(ctx, cfg.DiscordToken) }})
	}
	if cfg.TelegramToken != "" {
		steps = append(steps, step{"TELEGRAM_TOKEN", func() (string, error) { return s.TelegramToken(ctx, cfg.TelegramToken) }})
	}
	// TZ=UTC is what a container has when nobody chose: not an answer.
	if tz := strings.TrimSpace(os.Getenv("TZ")); tz != "" && tz != "UTC" {
		steps = append(steps, step{"TZ", func() (string, error) { return s.Timezone(ctx, tz) }})
	}
	daily, monthly := "", ""
	if v, ok := os.LookupEnv("THICKET_DAILY_BUDGET_USD"); ok {
		daily = v
	}
	if v, ok := os.LookupEnv("THICKET_MONTHLY_BUDGET_USD"); ok {
		monthly = v
	}
	if strings.TrimSpace(daily) != "" || strings.TrimSpace(monthly) != "" {
		steps = append(steps, step{"THICKET_*_BUDGET_USD", func() (string, error) { return s.Budgets(ctx, daily, monthly) }})
	}
	switch {
	case cfg.DiscordOwnerID != "":
		steps = append(steps, step{"THICKET_OWNER_DISCORD_ID", func() (string, error) { return s.Owner(ctx, "discord:"+cfg.DiscordOwnerID) }})
	case cfg.TelegramOwnerID != "":
		steps = append(steps, step{"THICKET_OWNER_TELEGRAM_ID", func() (string, error) { return s.Owner(ctx, "telegram:"+cfg.TelegramOwnerID) }})
	}
	if len(steps) == 0 {
		fmt.Fprintln(out, "Nothing to store: none of OPENROUTER_API_KEY, THICKET_MODEL_URL, DISCORD_TOKEN, TELEGRAM_TOKEN, TZ, THICKET_DAILY_BUDGET_USD, THICKET_MONTHLY_BUDGET_USD, THICKET_OWNER_DISCORD_ID or THICKET_OWNER_TELEGRAM_ID is set.")
	}
	var failed []string
	for _, st := range steps {
		msg, err := st.do()
		if err != nil {
			fmt.Fprintf(out, "FAILED  %s: %v\n", st.what, err)
			failed = append(failed, st.what)
			continue
		}
		fmt.Fprintf(out, "ok      %s\n", msg)
	}
	if len(failed) > 0 {
		return fmt.Errorf("not stored: %s", strings.Join(failed, ", "))
	}
	return nil
}

// asker reads answers a line at a time; secrets aren't echoed at a terminal.
type asker struct {
	in  *bufio.Reader
	tty *os.File
	out io.Writer
	eof bool
}

func (a *asker) line(prompt string) string {
	fmt.Fprint(a.out, prompt)
	if a.eof {
		fmt.Fprintln(a.out)
		return ""
	}
	s, err := a.in.ReadString('\n')
	if err != nil {
		a.eof = true // no more answers: everything after this is "keep"
		fmt.Fprintln(a.out)
	}
	return strings.TrimSpace(s)
}

func (a *asker) secret(prompt string) string {
	if a.tty == nil {
		return a.line(prompt)
	}
	fmt.Fprint(a.out, prompt)
	b, err := term.ReadPassword(int(a.tty.Fd()))
	fmt.Fprintln(a.out)
	if err != nil {
		a.eof = true
		return ""
	}
	return strings.TrimSpace(string(b))
}

// until asks until apply accepts the answer or the answer is empty
// (keep what's there). apply's message is printed either way.
func (a *asker) until(ask func() string, apply func(string) (string, error)) {
	for {
		v := ask()
		if v == "" {
			return
		}
		msg, err := apply(v)
		if err == nil {
			fmt.Fprintln(a.out, "  ok:", msg)
			return
		}
		fmt.Fprintln(a.out, "  That didn't work:", err)
		fmt.Fprintln(a.out, "  Try again, or press Enter to leave it.")
	}
}

func stored(has bool) string {
	if has {
		return "stored"
	}
	return "not set"
}

func initAsk(ctx context.Context, s *setup.Setup, cfg *config.Config, in io.Reader, out io.Writer) error {
	st, err := s.State(ctx)
	if err != nil {
		return err
	}
	a := &asker{in: bufio.NewReader(in), out: out}
	if f, ok := in.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		a.tty = f
	}
	fmt.Fprintf(out, "Setting up Thicket in %s.\nPress Enter to keep what's there or skip a question; everything can be changed later (run this again, or ask Juni). Keys and tokens are stored encrypted and never shown.\n", cfg.DataDir)
	if !s.Verify {
		fmt.Fprintln(out, "Keys and tokens will be stored without being checked (--no-verify).")
	}

	// 1. Where models run.
	now := "not set yet (Juni will ask for it in chat)"
	switch {
	case st.Local:
		now = "your own server at " + st.ModelURL + ", using " + st.Model
	case st.OpenRouterKey:
		now = "OpenRouter, key stored"
	}
	fmt.Fprintf(out, "\n1. The model. Now: %s.\n   1 = OpenRouter (a key from https://openrouter.ai/keys)\n   2 = your own server (Ollama, LM Studio, llama.cpp, vLLM)\n", now)
	switch a.line("   1, 2 or Enter: ") {
	case "1":
		a.until(func() string { return a.secret("   OpenRouter key: ") },
			func(v string) (string, error) { return s.OpenRouterKey(ctx, v) })
	case "2":
		fmt.Fprintln(out, "   The address usually ends in /v1, like http://192.168.1.20:11434/v1 (in Docker, this machine is http://host.docker.internal:11434/v1).")
		a.until(func() string { return a.line("   Server address: ") }, func(base string) (string, error) {
			apiKey := a.secret("   Its API key (most need none; Enter for none): ")
			model := ""
			if s.Verify {
				models, err := s.LocalModels(ctx, base, apiKey)
				if err != nil {
					return "", err
				}
				fmt.Fprintln(out, "   It has:", strings.Join(models, ", "))
				model = a.line("   Which one [" + models[0] + "]: ")
			} else {
				model = a.line("   Model name: ")
			}
			return s.LocalModel(ctx, base, model, apiKey)
		})
	}

	// 2 and 3. Chat apps.
	fmt.Fprintf(out, "\n2. Discord (optional). Bot token: %s.\n", stored(st.DiscordToken))
	a.until(func() string { return a.secret("   Discord bot token: ") },
		func(v string) (string, error) { return s.DiscordToken(ctx, v) })
	fmt.Fprintf(out, "\n3. Telegram (optional). Bot token: %s.\n", stored(st.TelegramToken))
	a.until(func() string { return a.secret("   Telegram bot token (from @BotFather): ") },
		func(v string) (string, error) { return s.TelegramToken(ctx, v) })

	// 4. Timezone.
	tzNow := st.Timezone
	if tzNow == "" {
		tzNow = "not set (Juni will ask each person where they are)"
	}
	fmt.Fprintf(out, "\n4. Timezone, for routines and quiet hours. Now: %s.\n", tzNow)
	a.until(func() string { return a.line("   Timezone (like Europe/London or America/Chicago): ") },
		func(v string) (string, error) { return s.Timezone(ctx, v) })

	// 5. Spend caps.
	fmt.Fprintf(out, "\n5. Spend caps in USD (0 = no cap). Now: %s a day, %s a month.\n", st.Daily, st.Monthly)
	a.until(func() string { return a.line("   Daily cap: ") },
		func(v string) (string, error) { return s.Budgets(ctx, v, "") })
	a.until(func() string { return a.line("   Monthly cap: ") },
		func(v string) (string, error) { return s.Budgets(ctx, "", v) })

	// 6. The owner.
	if st.Owner != "" {
		fmt.Fprintf(out, "\n6. The owner is %s.\n", st.Owner)
	} else {
		fmt.Fprintln(out, "\n6. The owner (optional). Without one, the first person to DM the bot, or to open the sign-in link below, becomes the owner.")
		a.until(func() string { return a.line("   Your account, as discord:USER_ID or telegram:USER_ID: ") },
			func(v string) (string, error) { return s.Owner(ctx, v) })
	}
	return nil
}

// initFinish says what's set and how to start.
func initFinish(ctx context.Context, s *setup.Setup, cfg *config.Config, out io.Writer) error {
	st, err := s.State(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, "\nThicket is set up:")
	switch {
	case st.Local:
		fmt.Fprintf(out, "  model      your own server at %s, using %s\n", st.ModelURL, st.Model)
	case st.OpenRouterKey:
		fmt.Fprintf(out, "  model      OpenRouter (%s)\n", st.Model)
	default:
		fmt.Fprintln(out, "  model      not set: Juni will ask for an OpenRouter key or a local server in your first chat")
	}
	fmt.Fprintf(out, "  discord    %s\n  telegram   %s\n", stored(st.DiscordToken), stored(st.TelegramToken))
	if st.Timezone != "" {
		fmt.Fprintf(out, "  timezone   %s\n", st.Timezone)
	}
	fmt.Fprintf(out, "  spend caps %s a day, %s a month\n", st.Daily, st.Monthly)
	if st.Owner != "" {
		fmt.Fprintf(out, "  owner      %s\n", st.Owner)
	}
	token, err := s.DB.CreateWebLink(ctx, 0, 24*time.Hour)
	if err != nil {
		return err
	}
	who := "signs you in as the owner"
	if st.Owner == "" {
		who = "makes whoever opens it the owner"
	}
	fmt.Fprintf(out, "\nStart Thicket:\n  docker compose up -d        (or: thicket serve)\n\nThen open the web app. This link works once, for 24 hours, and %s:\n  %s/login#%s\n", who, cfg.WebBaseURL(), token)
	if cfg.PublicURL == "" {
		fmt.Fprintln(out, "(Set THICKET_PUBLIC_URL if you reach Thicket through another address.)")
	}
	if st.DiscordToken || st.TelegramToken {
		fmt.Fprintln(out, "Or DM the bot: Juni says hello.")
	}
	fmt.Fprintln(out, "Check everything any time with: thicket doctor")
	return nil
}
