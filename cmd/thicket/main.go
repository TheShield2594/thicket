// Command thicket runs the Thicket agent platform.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/theshield2594/thicket/internal/app"
	"github.com/theshield2594/thicket/internal/backup"
	"github.com/theshield2594/thicket/internal/channel"
	"github.com/theshield2594/thicket/internal/channel/discord"
	"github.com/theshield2594/thicket/internal/channel/telegram"
	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/config"
	"github.com/theshield2594/thicket/internal/secrets"
	"github.com/theshield2594/thicket/internal/store"
)

// version is set at build time with -ldflags "-X main.version=..."
var version = "dev"

const usage = `thicket: a small, self-hosted family of AI companions.

Usage:
  thicket [serve]                     run Thicket (default)
  thicket check                       first-run check: say exactly what's missing
  thicket healthcheck                 exit 0 if the running server is healthy (for Docker)
  thicket export [-o FILE] [--include-key]
                                      export the database and character files
  thicket restore FILE [--force]      restore an export into the data directory
  thicket character list              list character files and any problems
  thicket character export ID [-o FILE]
  thicket character import FILE       add a shared character (integration access is stripped)
  thicket web link [--user ID] [--ttl 24h]
                                      print a one-time sign-in link for the web app
                                      (default: the owner; on a fresh install it makes you the owner)
  thicket skill list                  list saved skills (procedures sprigs follow)
  thicket skill export ID [-o FILE]   write a skill as Markdown, to share
  thicket skill import FILE           add a skill, or a new version of one with the same id
  thicket memory runs                 list recent automatic memory reviews
  thicket memory rollback RUN_ID      undo every change a memory review made
  thicket migrate                     apply pending database migrations (startup does this too) and list them
  thicket doctor                      check everything: database, secrets, model server, chat tokens,
                                      each MCP server, and the running server's live connections
  thicket costs [--week|--month|--days N] [--json]
                                      model spend by sprig (heartbeat vs task), model and day
  thicket voice-check SPRIG [--record]
                                      replay the sprig's voice tests and flag drift
  thicket inbox [list] | resolve ID   what needs you: failed or interrupted work, drafts, approvals
  thicket api-token create NAME [--user APP:ID]
                                      make a token for the local API (acts as the owner by default)
  thicket api-token list | revoke ID
  thicket ask [--sprig ID] [--conversation NAME] [--json] MESSAGE
                                      message a sprig through the running server and print the reply
                                      (MESSAGE or stdin; needs THICKET_API_TOKEN)
  thicket jobs                        list your jobs (needs THICKET_API_TOKEN)
  thicket run-job JOB_ID [--payload TEXT|-]
                                      run a job now, optionally with input (needs THICKET_API_TOKEN)
  thicket stop [--conversation NAME]  stop the reply in progress there, or else your running jobs
  thicket version

Configuration is through environment variables; see README.md.
`

func main() {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve()
	case "check":
		err = check()
	case "healthcheck":
		err = healthcheck()
	case "export":
		err = export(args)
	case "restore":
		err = restore(args)
	case "character", "characters":
		err = characterCmd(args)
	case "memory":
		err = memoryCmd(args)
	case "skill", "skills":
		err = skillCmd(args)
	case "web":
		err = webCmd(args)
	case "migrate":
		err = migrateCmd(os.Stdout)
	case "costs", "cost":
		err = costsCmd(args, os.Stdout)
	case "doctor":
		err = doctorCmd(os.Stdout)
	case "voice-check":
		err = voiceCheckCmd(args, os.Stdout)
	case "inbox":
		err = inboxCmd(args, os.Stdout)
	case "api-token", "api-tokens":
		err = apiTokenCmd(args)
	case "ask":
		err = askCmd(args)
	case "jobs":
		err = jobsCmd()
	case "run-job":
		err = runJobCmd(args)
	case "stop":
		err = stopCmd(args)
	case "version", "--version", "-v":
		fmt.Println("thicket", version)
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		fmt.Fprint(os.Stderr, usage)
		err = fmt.Errorf("unknown command %q", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func loadConfig() *config.Config {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "configuration error:", err)
		os.Exit(2)
	}
	return cfg
}

func printProblems(probs []config.Problem) (fatal bool) {
	for _, p := range probs {
		mark := "note"
		if p.Fatal {
			mark, fatal = "MISSING", true
		}
		fmt.Fprintf(os.Stderr, "[%s] %s\n        %s\n", mark, p.What, p.Fix)
	}
	return fatal
}

func check() error {
	cfg := loadConfig()
	if printProblems(cfg.Check()) {
		return errors.New("Thicket can't start until the items marked MISSING are fixed")
	}
	fmt.Println("All required settings are present. Data directory:", cfg.DataDir)
	return nil
}

func serve() error {
	cfg := loadConfig()
	if printProblems(cfg.Check()) {
		return errors.New("Thicket can't start until the items marked MISSING are fixed (run `thicket check` any time)")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	a, err := app.New(ctx, cfg, version, app.Options{Channels: func(log *slog.Logger) []channel.Channel {
		var chs []channel.Channel
		if cfg.DiscordToken != "" {
			chs = append(chs, discord.New(cfg.DiscordToken, cfg.DiscordListenChannels, log))
		}
		if cfg.TelegramToken != "" {
			chs = append(chs, telegram.New(cfg.TelegramToken, cfg.TelegramListenChats, log))
		}
		return chs // none: the web app alone
	}})
	if err != nil {
		return err
	}
	defer a.Close()
	return a.Run(ctx)
}

func healthcheck() error {
	cfg := loadConfig()
	addr := cfg.HTTPAddr
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get("http://" + addr + "/health")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// Read it whole (1 MB is far beyond any /health answer): a cut-off
	// body can't be parsed for the setup exception below.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20+1))
	if err != nil {
		return err
	}
	if len(body) > 1<<20 {
		return errors.New("the /health answer is too large")
	}
	fmt.Println(strings.TrimSpace(string(body)))
	// A missing model key is a setup state, not a crash: don't mark the
	// container unhealthy for it alone.
	if resp.StatusCode != http.StatusOK && !onlyModelKeyMissing(string(body)) {
		return fmt.Errorf("unhealthy (HTTP %d)", resp.StatusCode)
	}
	return nil
}

// onlyModelKeyMissing reports whether the one required check failing is
// the model key (optional checks never count).
func onlyModelKeyMissing(body string) bool {
	var h struct {
		Checks map[string]struct {
			OK       bool `json:"ok"`
			Optional bool `json:"optional"`
		} `json:"checks"`
	}
	if json.Unmarshal([]byte(body), &h) != nil {
		return false
	}
	failed := []string{}
	for name, c := range h.Checks {
		if !c.OK && !c.Optional {
			failed = append(failed, name)
		}
	}
	return len(failed) == 1 && failed[0] == "model_key"
}

func openStore(cfg *config.Config) (*store.Store, error) {
	return store.Open(context.Background(), filepath.Join(cfg.DataDir, "thicket.db"))
}

func export(args []string) error {
	fs := flag.NewFlagSet("export", flag.ContinueOnError)
	out := fs.String("o", "", "output file (default thicket-export-DATE.tar.gz)")
	withKey := fs.Bool("include-key", false, "include secret.key so stored API keys survive the move (keep the file safe!)")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return errors.New("usage: thicket export [-o FILE] [--include-key]")
	}
	cfg := loadConfig()
	db, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	path := *out
	if path == "" {
		path = "thicket-export-" + time.Now().Format("20060102-150405") + ".tar.gz"
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if err := backup.Export(context.Background(), db, cfg.DataDir, cfg.CharactersDir, *withKey, f); err != nil {
		f.Close()
		os.Remove(path)
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	fmt.Println("exported to", path)
	if !*withKey {
		fmt.Println("note: secret.key was not included; stored API keys will need re-entering after a restore unless you keep THICKET_SECRET_KEY or the key file.")
	}
	return nil
}

func restore(args []string) error {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	force := fs.Bool("force", false, "replace an existing database (the old files are kept as .before-restore-*)")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: thicket restore FILE [--force]")
	}
	cfg := loadConfig()
	f, err := os.Open(pos[0])
	if err != nil {
		return err
	}
	defer f.Close()
	if err := backup.Restore(f, cfg.DataDir, cfg.CharactersDir, *force); err != nil {
		return err
	}
	db, err := openStore(cfg) // runs any migrations the restored DB needs
	if err != nil {
		return fmt.Errorf("restored, but the database didn't open: %w", err)
	}
	v, _ := db.SchemaVersion(context.Background())
	db.Close()
	fmt.Println("restored into", cfg.DataDir, "- schema version", v)
	return nil
}

func characterCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: thicket character list|export|import")
	}
	cfg := loadConfig()
	switch args[0] {
	case "list":
		db, err := openStore(cfg)
		if err != nil {
			return err
		}
		defer db.Close()
		reg := character.NewRegistry(cfg.CharactersDir, db, app.NewLogger("error", io.Discard, secrets.NewRedactor()))
		if err := reg.Load(); err != nil {
			return err
		}
		for _, c := range reg.All() {
			host := ""
			if c.Host {
				host = " (host)"
			}
			fmt.Printf("%-12s %s%s: %s  tools=%s\n", c.ID, c.DisplayName(), host, c.Role, strings.Join(c.Tools, ","))
		}
		for _, e := range reg.Errors() {
			fmt.Println("PROBLEM:", e.Error())
		}
		return nil
	case "export":
		fs := flag.NewFlagSet("character export", flag.ContinueOnError)
		out := fs.String("o", "", "output file")
		pos, err := parseFlags(fs, args[1:])
		if err != nil {
			return err
		}
		if len(pos) != 1 {
			return errors.New("usage: thicket character export ID [-o FILE]")
		}
		id := pos[0]
		b, err := os.ReadFile(filepath.Join(cfg.CharactersDir, filepath.Base(id)+".md"))
		if err != nil {
			return err
		}
		if _, err := character.Parse(b); err != nil {
			return err
		}
		path := *out
		if path == "" {
			path = id + ".md"
		}
		if err := os.WriteFile(path, b, 0o644); err != nil {
			return err
		}
		fmt.Println("exported", id, "to", path, "(memories are never included)")
		return nil
	case "import":
		if len(args) != 2 {
			return errors.New("usage: thicket character import FILE")
		}
		c, stripped, err := backup.ImportCharacter(args[1], cfg.CharactersDir)
		if err != nil {
			return err
		}
		fmt.Printf("imported %s (%s). A running Thicket picks it up within 10 seconds.\n", c.DisplayName(), c.ID)
		if len(stripped) > 0 {
			fmt.Println("for safety these were removed and can be granted in chat:", strings.Join(stripped, ", "))
		}
		return nil
	}
	return fmt.Errorf("unknown character command %q", args[0])
}

func memoryCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: thicket memory runs|rollback RUN_ID")
	}
	cfg := loadConfig()
	db, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	ctx := context.Background()
	switch args[0] {
	case "runs":
		runs, err := db.ListExtractionRuns(ctx, 30)
		if err != nil {
			return err
		}
		for _, r := range runs {
			rb := ""
			if !r.RolledBackAt.IsZero() {
				rb = " ROLLED BACK"
			}
			fmt.Printf("%5d  %s  user=%d  %-5s applied=%d skipped=%d%s %s\n", r.ID, r.CreatedAt.Format("2006-01-02 15:04"), r.UserID, r.Status, r.Applied, r.Skipped, rb, firstLine(r.Error))
		}
		return nil
	case "rollback":
		if len(args) != 2 {
			return errors.New("usage: thicket memory rollback RUN_ID")
		}
		id, err := strconv.ParseInt(args[1], 10, 64)
		if err != nil {
			return err
		}
		n, err := db.RollbackRun(ctx, id)
		if err != nil {
			return err
		}
		fmt.Printf("rolled back run %d (%d changes undone)\n", id, n)
		return nil
	}
	return fmt.Errorf("unknown memory command %q", args[0])
}

// parseFlags parses flags wherever they appear, so the documented
// "thicket restore FILE --force" works as well as "--force FILE".
// The standard flag package stops at the first positional argument.
func parseFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		// "--" ends flag parsing for good: everything after it is positional.
		if used := len(args) - len(rest); used > 0 && args[used-1] == "--" {
			return append(pos, rest...), nil
		}
		if len(rest) == 0 {
			return pos, nil
		}
		pos = append(pos, rest[0])
		args = rest[1:]
	}
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return s
}

func webCmd(args []string) error {
	if len(args) == 0 || args[0] != "link" {
		return errors.New("usage: thicket web link [--user ID] [--ttl 24h]")
	}
	fs := flag.NewFlagSet("web link", flag.ContinueOnError)
	userID := fs.Int64("user", 0, "Thicket user id to sign in as (default: the owner)")
	ttl := fs.Duration("ttl", 24*time.Hour, "how long the link stays valid")
	pos, err := parseFlags(fs, args[1:])
	if err != nil {
		return err
	}
	if len(pos) != 0 || *ttl <= 0 || *ttl > 7*24*time.Hour {
		return errors.New("usage: thicket web link [--user ID] [--ttl 24h] (ttl up to 168h)")
	}
	cfg := loadConfig()
	db, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	ctx := context.Background()
	who := "the owner"
	if *userID != 0 {
		u, err := db.UserByID(ctx, *userID)
		if err != nil {
			return fmt.Errorf("user %d: %w", *userID, err)
		}
		who = fmt.Sprintf("user %d (%s)", u.ID, u.Role)
	} else if has, err := db.HasOwner(ctx); err != nil {
		return err
	} else if !has {
		who = "a new owner (nobody owns this Thicket yet)"
	}
	token, err := db.CreateWebLink(ctx, *userID, *ttl)
	if err != nil {
		return err
	}
	fmt.Printf("Sign-in link for %s, valid once for %s:\n\n  %s/login#%s\n\n", who, *ttl, cfg.WebBaseURL(), token)
	if cfg.PublicURL == "" {
		fmt.Println("Set THICKET_PUBLIC_URL if you reach Thicket through another address.")
	}
	return nil
}
