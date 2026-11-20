package main

import (
	"bufio"
	"bytes"
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
       thicket export-sprig ID --for-gallery [-o FILE] [--skill ID]...

Writes one shareable file: the sprig's character, routines, the skills it
mentions (add others with --skill), avatar and voice tests. Secrets, chats,
the trail, the inbox and permission levels never go in.

Memories go in only with --memories: what the sprig knows between it and
one person (the owner, or --user ID). --shared adds that person's shared
memories, --family the household's family memory. Leave them out of a
package you give to someone else.

--for-gallery writes a signed package for the gallery instead: only the
character, its drawn avatar, voice tests and skills (its own, and the shared
ones it mentions), signed with your author key (thicket gallery keygen).
No routines, memories or pictures; its tools must be whole scopes.`

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
	forGallery := fs.Bool("for-gallery", false, "")
	pos, err := parseFlags(fs, args)
	if err != nil || len(pos) != 1 {
		return errors.New(exportSprigUsage)
	}
	if *forGallery {
		if *withMem || *withShared || *withFamily || *userID != 0 {
			return errors.New("a gallery package never carries memories: drop --memories, --shared, --family and --user")
		}
		return exportGallery(cfg, pos[0], *file, extra, out)
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

// exportGallery writes a signed gallery package.
func exportGallery(cfg *config.Config, id, file string, extra []string, out io.Writer) error {
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
	sec, err := openSecrets(ctx, cfg, db)
	if err != nil {
		return err
	}
	priv, err := gallerySigningKey(ctx, sec)
	if err != nil {
		return err
	}
	id = strings.ToLower(id)
	if c, ok := findSprigFile(cfg, db, id); ok {
		id = c.ID
	}
	lib, err := db.Skills(ctx)
	if err != nil {
		return err
	}
	g, err := sprigpkg.ExportGallery(sprigpkg.GallerySource{CharsDir: cfg.CharactersDir, ID: id, Library: lib, ExtraSkills: extra, Version: version, LooksSecret: looks})
	if err != nil {
		return err
	}
	b, err := g.Bytes(priv)
	if err != nil {
		return err
	}
	if file == "" {
		file = id + sprigpkg.Ext
	}
	if err := writeNew(file, b); err != nil {
		return err
	}
	fmt.Fprintf(out, "Exported %s for the gallery to %s: %d voice tests, %d skills, signed by author key %s.\n", g.Char.DisplayName(), file,
		countPrefix(g.Files, "voice_tests/"), len(g.Skills), g.Signer.KeyID)
	fmt.Fprintf(out, "Fingerprint: %s\n", g.Signer.Fingerprint())
	for _, o := range g.Omitted {
		fmt.Fprintln(out, "Left out:", o)
	}
	fmt.Fprintln(out, "No routines, memories, pictures, secrets, chats or permission levels are in it.")
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
	raw, err := readPackageFile(pos[0])
	if err != nil {
		return err
	}
	if sprigpkg.IsGallery(raw) {
		if *memMode != "" && *memMode != "skip" {
			return errors.New("a gallery package carries no memories")
		}
		return importGalleryCmd(cfg, pos[0], raw, *id, *name, *userID, *yes, in, out)
	}
	p, err := sprigpkg.Read(bytes.NewReader(raw))
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

// readPackageFile reads a package file, refusing anything far larger than
// a package can be.
func readPackageFile(name string) ([]byte, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 64<<20+1))
	if err != nil {
		return nil, err
	}
	if len(b) > 64<<20 {
		return nil, errors.New("that file is far too large to be a sprig package")
	}
	return b, nil
}

// importGalleryCmd imports a signed gallery package, or updates a sprig
// that came from the same author.
func importGalleryCmd(cfg *config.Config, file string, raw []byte, id, name string, userID int64, yes bool, in io.Reader, out io.Writer) error {
	db, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	ctx := context.Background()
	revoked, err := sprigpkg.RevokedKeys(ctx, db)
	if err != nil {
		return err
	}
	g, err := sprigpkg.ReadGallery(bytes.NewReader(raw), revoked)
	if err != nil {
		return err
	}
	if id != "" {
		// A new id for a new sprig; an update keeps the one it has.
		g.Char.ID = strings.ToLower(strings.TrimSpace(id))
	}
	trust, err := g.CheckTrust(ctx, db, cfg.CharactersDir, revoked)
	if err != nil {
		return err
	}
	if err := g.Prepare("", name, trust.Update); err != nil {
		return err
	}
	if !trust.Update {
		reg := loadRegistry(cfg, db)
		taken := func(s string) bool { _, ok := reg.FindByName(s); return ok }
		if err := sprigpkg.CheckClash(g.Char, cfg.CharactersDir, taken); err != nil {
			var c *sprigpkg.Clash
			if errors.As(err, &c) && c.Suggest != "" {
				return fmt.Errorf("%w: pass --%s %q", err, c.What, c.Suggest)
			}
			return err
		}
	}
	if err := g.PlanSkills(ctx, db, agent.ValidateSkill); err != nil {
		return err
	}
	preview, err := g.Preview(trust)
	if err != nil {
		return err
	}
	verb, ask, done := "Importing", "Import", "imported"
	if trust.Update {
		verb, ask, done = "Updating", "Update", "updated"
	}
	if _, err := fmt.Fprintf(out, "%s %s from the gallery package %s (made with Thicket %s). This is everything it will get:\n\n%s\n", verb, g.Char.DisplayName(), file, orUnknown(g.Manifest.ThicketVersion), preview); err != nil {
		return fmt.Errorf("couldn't show the preview, so nothing was imported: %w", err)
	}
	if !yes {
		fmt.Fprintf(out, "%s %s? [y/N] ", ask, g.Char.DisplayName())
		line, _ := bufio.NewReader(in).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			fmt.Fprintln(out, "Nothing was written.")
			return nil
		}
	}
	var by int64
	if u, err := memoryPerson(ctx, db, userID); err == nil {
		by = u.ID
	}
	res, err := g.Install(ctx, db, cfg.CharactersDir, sprigpkg.GalleryInstallOptions{By: by, Update: trust.Update})
	if err != nil {
		return err
	}
	_ = db.AddTrail(ctx, store.TrailEntry{UserID: by, Character: g.Char.ID, Kind: store.TrailGrant,
		Summary: fmt.Sprintf("%s %s from the gallery (author key %s); no scopes granted, tools at draft", done, g.Char.DisplayName(), g.Signer.KeyID)})
	if trust.Update {
		fmt.Fprintf(out, "Updated %s (%s). A running Thicket picks it up within 10 seconds.\n", g.Char.DisplayName(), g.Char.ID)
	} else {
		fmt.Fprintf(out, "Imported %s (%s) with no scopes granted and every tool at draft. A running Thicket picks it up within 10 seconds.\n", g.Char.DisplayName(), g.Char.ID)
	}
	if len(res.Pending) > 0 {
		fmt.Fprintf(out, "Skills waiting for your approval: %s (thicket skill approve %s ID, or ask in chat).\n", strings.Join(res.Pending, ", "), g.Char.ID)
	}
	if len(g.Asks) > 0 && !trust.Update {
		fmt.Fprintln(out, "It asks for:", strings.Join(g.Asks, ", ")+". Grant any you want in chat.")
	}
	return nil
}
