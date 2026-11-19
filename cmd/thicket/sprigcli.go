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

	"github.com/theshield2594/thicket/internal/agent"
	"github.com/theshield2594/thicket/internal/app"
	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/config"
	"github.com/theshield2594/thicket/internal/secrets"
	"github.com/theshield2594/thicket/internal/sprigpkg"
	"github.com/theshield2594/thicket/internal/store"
)

const exportSprigUsage = `usage: thicket export-sprig ID [-o FILE] [--skill ID]... [--memories [--shared] [--family]] [--user ID]

Writes one shareable file: the sprig's character, routines, the skills it
mentions (add others with --skill), avatar and voice tests. Secrets, chats,
the trail, the inbox and permission levels never go in.

Memories go in only with --memories: what the sprig knows between it and
one person (the owner, or --user ID). --shared adds that person's shared
memories, --family the household's family memory. Leave them out of a
package you give to someone else.`

const importSprigUsage = `usage: thicket import-sprig FILE [--id ID] [--name NAME] [--user ID] [--memories locked|review|skip] [--yes]

Shows everything the sprig will get before anything is written. Every tool
starts at draft; routines are scheduled for the owner (or --user).

A package with memories lists them first. They're kept for the owner (or
--user) either locked (as if that person set them: automatic review never
changes them) or reviewable (the memory review may update or expire them),
or skipped. Without --memories you're asked; with --yes they're skipped.`

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func exportSprigCmd(cfg *config.Config, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("export-sprig", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	file := fs.String("o", "", "")
	var extra multiFlag
	fs.Var(&extra, "skill", "")
	withMem := fs.Bool("memories", false, "")
	withShared := fs.Bool("shared", false, "")
	withFamily := fs.Bool("family", false, "")
	userID := fs.Int64("user", 0, "")
	pos, err := parseFlags(fs, args)
	if err != nil || len(pos) != 1 {
		return errors.New(exportSprigUsage)
	}
	if (*withShared || *withFamily || *userID != 0) && !*withMem {
		return errors.New("--shared, --family and --user choose which memories go in: add --memories\n\n" + exportSprigUsage)
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
	var mems []store.Memory
	var who *store.User
	if *withMem {
		if who, err = memoryPerson(ctx, db, *userID); err != nil {
			return err
		}
		visible, err := db.VisibleMemories(ctx, who.ID, id)
		if err != nil {
			return err
		}
		for _, m := range visible {
			if m.Character == id || *withShared {
				mems = append(mems, m)
			}
		}
		if *withFamily {
			if who.IsKid() {
				return errors.New("family memory goes in only with an adult's memories")
			}
			fam, err := db.FamilyMemories(ctx, id)
			if err != nil {
				return err
			}
			mems = append(mems, fam...)
		}
	}
	p, err := sprigpkg.Export(sprigpkg.Source{CharsDir: cfg.CharactersDir, ID: id, Jobs: jobs, Library: lib, ExtraSkills: extra, Version: version, LooksSecret: looks, Memories: mems})
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
	if err := writeNew(path, b); err != nil {
		return err
	}
	fmt.Fprintf(out, "Exported %s to %s: %d voice tests, %d routines, %d skills%s.\n", p.Char.DisplayName(), path,
		countPrefix(p.Files, "voice_tests/"), len(p.Routines), len(p.Skills), map[bool]string{true: ", a picture", false: ""}[p.Avatar != nil])
	for _, o := range p.Omitted {
		fmt.Fprintln(out, "Left out:", o)
	}
	if len(p.Memories) > 0 {
		fmt.Fprintf(out, "It holds %d of %s's memories: share it only with someone who should have them. No secrets, chats or permission levels are in it.\n", len(p.Memories), userLabel(who))
	} else {
		fmt.Fprintln(out, "No memories, secrets, chats or permission levels are in it.")
	}
	return nil
}

// memoryPerson is whose memories an export or import is about: --user, or
// the owner.
func memoryPerson(ctx context.Context, db *store.Store, userID int64) (*store.User, error) {
	if userID != 0 {
		u, err := db.UserByID(ctx, userID)
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("there's no user %d", userID)
		}
		return u, err
	}
	u, err := db.Owner(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return nil, errors.New("there's no owner yet to keep memories for; finish setup first, or pass --user ID")
	}
	return u, err
}

// writeNew creates path, refusing to replace a file that's there, and
// removes what it wrote if writing fails.
func writeNew(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if os.IsExist(err) {
		return fmt.Errorf("%s already exists; move it, or choose another file with -o", path)
	}
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
	}
	return err
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
	looks := func(s string) bool { return red.Redact(s) != s }
	names, err := db.SecretNames(ctx)
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return looks, nil // nothing stored: credential shapes are all there is to check
	}
	fail := func(err error) error {
		return fmt.Errorf("couldn't read the stored secrets to check the package against, so nothing was exported: %w", err)
	}
	key, _, err := secrets.ResolveKey(cfg.SecretKey, cfg.SecretKeyFile, cfg.DataDir)
	if err != nil {
		return nil, fail(err)
	}
	sec, err := secrets.New(db, key, red)
	if err != nil {
		return nil, fail(err)
	}
	if err := sec.WarmRedactor(ctx); err != nil {
		return nil, fail(err)
	}
	return looks, nil
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
	memMode := fs.String("memories", "", "")
	pos, err := parseFlags(fs, args)
	if err != nil || len(pos) != 1 {
		return errors.New(importSprigUsage)
	}
	switch *memMode {
	case "", "locked", "review", "skip":
	default:
		return errors.New("--memories is locked, review or skip\n\n" + importSprigUsage)
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
		if errors.As(err, &c) && c.Suggest != "" {
			return fmt.Errorf("%w: pass --%s %q", err, c.What, c.Suggest)
		}
		if errors.As(err, &c) {
			return fmt.Errorf("%w: pass --%s", err, c.What)
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
	plans, err := p.PlanSkills(ctx, db, agent.ValidateSkill)
	if err != nil {
		return err
	}
	// The CLI runs on the server with access to the data: it's the owner's.
	preview, err := p.Preview(plans, stripped, true)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "Importing %s from %s (made with Thicket %s). This is everything it will get:\n\n%s\n%s", p.Char.DisplayName(), pos[0], orUnknown(p.Manifest.ThicketVersion), preview, p.MemoryPreview()); err != nil {
		return fmt.Errorf("couldn't show the preview, so nothing was imported: %w", err)
	}
	if len(p.Memories) > 0 && owner == nil && *memMode != "skip" && *memMode != "" {
		return errors.New("there's no user to keep the memories for yet; finish setup first, pass --user ID, or --memories skip")
	}
	stdin := bufio.NewReader(in)
	if !*yes {
		fmt.Fprintf(out, "Import %s? [y/N] ", p.Char.DisplayName())
		line, _ := stdin.ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			fmt.Fprintln(out, "Nothing was written.")
			return nil
		}
	}
	mode := *memMode
	switch {
	case len(p.Memories) == 0 || owner == nil:
		mode = "skip"
	case mode == "" && *yes:
		mode = "skip"
	case mode == "":
		fmt.Fprintf(out, "Keep its %d memories for %s locked (as if %s set them), reviewable (the memory review may change them), or skip them? [l/r/S] ", len(p.Memories), userLabel(owner), userLabel(owner))
		line, _ := stdin.ReadString('\n')
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "l", "locked":
			mode = "locked"
		case "r", "review", "reviewable":
			mode = "review"
		default:
			mode = "skip"
		}
	}
	var ownerID int64
	if owner != nil {
		ownerID = owner.ID
	}
	res, err := p.Install(ctx, db, cfg.CharactersDir, sprigpkg.InstallOptions{Owner: ownerID, Plans: plans, CanWriteSkills: true})
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
	if len(p.Memories) > 0 {
		if mode == "skip" {
			fmt.Fprintln(out, "Memories: none kept.")
			return nil
		}
		mr, err := p.InstallMemories(ctx, db, owner.ID, mode == "locked", !owner.IsKid(), time.Now())
		if err != nil {
			return fmt.Errorf("the sprig was imported, but its memories weren't saved: %w", err)
		}
		how := map[string]string{"locked": "locked", "review": "reviewable"}[mode]
		fmt.Fprintf(out, "Memories: %d kept for %s, %s.\n", mr.Added, userLabel(owner), how)
		for _, s := range mr.Skipped {
			fmt.Fprintln(out, "Not kept:", s)
		}
	}
	return nil
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
