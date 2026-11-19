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

	"github.com/theshield2594/thicket/internal/agent"
	"github.com/theshield2594/thicket/internal/app"
	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/config"
	"github.com/theshield2594/thicket/internal/secrets"
	"github.com/theshield2594/thicket/internal/sprigpkg"
	"github.com/theshield2594/thicket/internal/store"
)

const exportSprigUsage = `usage: thicket export-sprig ID [-o FILE] [--skill ID]...

Writes one shareable file: the sprig's character, routines, the skills it
mentions (add others with --skill), avatar and voice tests. Memories,
secrets, chats, the trail, the inbox and permission levels never go in.`

const importSprigUsage = `usage: thicket import-sprig FILE [--id ID] [--name NAME] [--user ID] [--yes]

Shows everything the sprig will get before anything is written. Every tool
starts at draft; routines are scheduled for the owner (or --user).`

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func exportSprigCmd(cfg *config.Config, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("export-sprig", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	file := fs.String("o", "", "")
	var extra multiFlag
	fs.Var(&extra, "skill", "")
	pos, err := parseFlags(fs, args)
	if err != nil || len(pos) != 1 {
		return errors.New(exportSprigUsage)
	}
	db, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	ctx := context.Background()
	looks, err := secretCheck(ctx, cfg, db)
	if err != nil {
		return err
	}
	id := strings.ToLower(pos[0])
	if c, ok := findSprigFile(cfg, db, id); ok {
		id = c.ID
	}
	jobs, err := db.CharacterJobs(ctx, id)
	if err != nil {
		return err
	}
	lib, err := db.Skills(ctx)
	if err != nil {
		return err
	}
	p, err := sprigpkg.Export(sprigpkg.Source{CharsDir: cfg.CharactersDir, ID: id, Jobs: jobs, Library: lib, ExtraSkills: extra, Version: version, LooksSecret: looks})
	if err != nil {
		return err
	}
	b, err := p.Bytes()
	if err != nil {
		return err
	}
	path := *file
	if path == "" {
		path = id + sprigpkg.Ext
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(out, "Exported %s to %s: %d voice tests, %d routines, %d skills%s.\n", p.Char.DisplayName(), path,
		countPrefix(p.Files, "voice_tests/"), len(p.Routines), len(p.Skills), map[bool]string{true: ", a picture", false: ""}[p.Avatar != nil])
	fmt.Fprintln(out, "No memories, secrets, chats or permission levels are in it.")
	return nil
}

func countPrefix(m map[string][]byte, prefix string) int {
	n := 0
	for k := range m {
		if strings.HasPrefix(k, prefix) {
			n++
		}
	}
	return n
}

// secretCheck reports text that looks like a credential, including the
// values of this install's stored secrets.
func secretCheck(ctx context.Context, cfg *config.Config, db *store.Store) (func(string) bool, error) {
	red := secrets.NewRedactor()
	if key, _, err := secrets.ResolveKey(cfg.SecretKey, cfg.SecretKeyFile, cfg.DataDir); err == nil {
		if sec, err := secrets.New(db, key, red); err == nil {
			if err := sec.WarmRedactor(ctx); err != nil {
				return nil, fmt.Errorf("couldn't read the stored secrets to check the package against: %w", err)
			}
		}
	}
	return func(s string) bool { return red.Redact(s) != s }, nil
}

func loadRegistry(cfg *config.Config, db *store.Store) *character.Registry {
	reg := character.NewRegistry(cfg.CharactersDir, db, app.NewLogger("error", io.Discard, secrets.NewRedactor()))
	_ = reg.Load()
	return reg
}

func findSprigFile(cfg *config.Config, db *store.Store, name string) (*character.Character, bool) {
	reg := loadRegistry(cfg, db)
	if c, ok := reg.Get(name); ok {
		return c, true
	}
	return reg.FindByName(name)
}

func importSprigCmd(cfg *config.Config, args []string, in io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("import-sprig", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	id := fs.String("id", "", "")
	name := fs.String("name", "", "")
	userID := fs.Int64("user", 0, "")
	yes := fs.Bool("yes", false, "")
	pos, err := parseFlags(fs, args)
	if err != nil || len(pos) != 1 {
		return errors.New(importSprigUsage)
	}
	p, err := sprigpkg.ReadFile(pos[0])
	if err != nil {
		return err
	}
	stripped, err := p.Prepare(*id, *name)
	if err != nil {
		return err
	}
	db, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	ctx := context.Background()
	reg := loadRegistry(cfg, db)
	taken := func(s string) bool { _, ok := reg.FindByName(s); return ok }
	if err := sprigpkg.CheckClash(p.Char, cfg.CharactersDir, taken); err != nil {
		var c *sprigpkg.Clash
		if errors.As(err, &c) {
			return fmt.Errorf("%w: pass --%s %q", err, c.What, c.Suggest)
		}
		return err
	}
	var owner *store.User
	if *userID != 0 {
		owner, err = db.UserByID(ctx, *userID)
	} else {
		owner, err = db.Owner(ctx)
	}
	if errors.Is(err, store.ErrNotFound) {
		if len(p.Routines) > 0 {
			return errors.New("there's no user to run the sprig's routines for yet; finish setup first, or pass --user ID")
		}
		owner, err = nil, nil
	}
	if err != nil {
		return err
	}
	plans, err := p.PlanSkills(ctx, db)
	if err != nil {
		return err
	}
	preview, err := p.Preview(plans, stripped)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Importing %s from %s (made with Thicket %s). This is everything it will get:\n\n%s\n", p.Char.DisplayName(), pos[0], orUnknown(p.Manifest.ThicketVersion), preview)
	if !*yes {
		fmt.Fprintf(out, "Import %s? [y/N] ", p.Char.DisplayName())
		line, _ := bufio.NewReader(in).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			fmt.Fprintln(out, "Nothing was written.")
			return nil
		}
	}
	var ownerID int64
	if owner != nil {
		ownerID = owner.ID
	}
	res, err := p.Install(ctx, db, cfg.CharactersDir, ownerID, agent.ValidateSkill)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Imported %s (%s), with every tool at draft. A running Thicket picks it up within 10 seconds.\n", p.Char.DisplayName(), p.Char.ID)
	if len(res.Routines) > 0 {
		fmt.Fprintf(out, "Routines scheduled for %s: %d.\n", userLabel(owner), len(res.Routines))
	}
	if len(res.Skills) > 0 {
		fmt.Fprintln(out, "Skills:", strings.Join(res.Skills, ", "))
	}
	return nil
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
