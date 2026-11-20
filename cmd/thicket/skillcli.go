package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/theshield2594/thicket/internal/agent"
	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/store"
)

func skillCmd(args []string) error {
	if len(args) == 0 {
		return errors.New(skillUsage)
	}
	// check reads files only, so it works without a data directory.
	if args[0] == "check" {
		return skillCheck(args[1:])
	}
	cfg := loadConfig()
	db, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	ctx := context.Background()
	switch args[0] {
	case "list":
		skills, err := db.Skills(ctx)
		if err != nil {
			return err
		}
		if len(skills) == 0 {
			fmt.Println("no skills")
		}
		for _, k := range skills {
			owner := ""
			if !k.Shared() {
				owner = " [" + k.SprigID + " only]"
			}
			fmt.Printf("%-24s v%-3d %s%s: %s\n", k.ID, k.Version, k.Name, owner, k.Description)
		}
		return nil
	case "export":
		fs := flag.NewFlagSet("skill export", flag.ContinueOnError)
		out := fs.String("o", "", "output file (default ID.skill.md)")
		pos, err := parseFlags(fs, args[1:])
		if err != nil {
			return err
		}
		if len(pos) != 1 {
			return errors.New("usage: thicket skill export ID [-o FILE]")
		}
		k, err := db.SkillByID(ctx, pos[0], 0)
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("no skill %q", pos[0])
		} else if err != nil {
			return err
		}
		b, err := agent.FormatSkill(*k)
		if err != nil {
			return err
		}
		if *out == "" {
			*out = k.ID + ".skill.md"
		}
		if *out == "-" {
			_, err = os.Stdout.Write(b)
			return err
		}
		if err := os.WriteFile(*out, b, 0o644); err != nil {
			return err
		}
		fmt.Printf("wrote %s (version %d)\n", *out, k.Version)
		return nil
	case "import":
		if len(args) != 2 {
			return errors.New("usage: thicket skill import FILE")
		}
		b, err := os.ReadFile(args[1])
		if err != nil {
			return err
		}
		k, err := agent.ParseSkill(b)
		if err != nil {
			return fmt.Errorf("%s: %w", args[1], err)
		}
		k.Author, k.Note = "import", "imported from "+strings.TrimSpace(args[1])
		base := 0
		if cur, err := db.SkillByID(ctx, k.ID, 0); err == nil {
			if !cur.Shared() {
				return fmt.Errorf("%s is %s's own skill: edit characters/%s/skills/%s.md instead (Thicket picks the change up by itself)", k.ID, cur.SprigID, cur.SprigID, k.ID)
			}
			base = cur.Version // an existing skill gets a new version; the old ones stay
		}
		v, err := db.SaveSkill(ctx, k, base)
		if errors.Is(err, store.ErrNoChange) {
			fmt.Printf("skill %s is already exactly that (version %d)\n", k.ID, v)
			return nil
		}
		if err != nil {
			return err
		}
		fmt.Printf("imported skill %s as version %d\n", k.ID, v)
		printHints(k)
		return nil
	case "starter":
		return skillStarter(ctx, db, args[1:])
	case "promote":
		if len(args) != 2 {
			return errors.New("usage: thicket skill promote ID")
		}
		return skillPromote(ctx, cfg.CharactersDir, db, args[1])
	}
	return fmt.Errorf("unknown skill command %q", args[0])
}

const skillUsage = "usage: thicket skill list | export ID [-o FILE] | import FILE | check FILE... | starter [ID... | all] | promote ID"

// skillPromote moves a sprig's own skill into the shared library. Its file
// is kept as <id>.md.promoted so the sprig doesn't take it back on the
// next load.
func skillPromote(ctx context.Context, charsDir string, db *store.Store, id string) error {
	k, err := db.SkillByID(ctx, id, 0)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("no skill %q", id)
	} else if err != nil {
		return err
	}
	if k.Shared() {
		return fmt.Errorf("%s is already in the shared library", id)
	}
	// Shared first: a running server that saw the file go before that
	// would delete the skill as the sprig's.
	// The trail entry goes in with the promotion, or neither does.
	if err := db.PromoteSkill(ctx, id, &store.TrailEntry{Character: k.SprigID, Kind: store.TrailSkill,
		Summary: fmt.Sprintf("skill %s moved from %s's own skills to the shared library", id, k.SprigID), Detail: "thicket skill promote"}); err != nil {
		return err
	}
	path := filepath.Join(character.SkillDir(charsDir, k.SprigID), id+".md")
	if err := os.Rename(path, path+".promoted"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%s is shared now, but its file couldn't be moved aside (%v): rename %s yourself", id, err, path)
	}
	fmt.Printf("%s is in the shared library now: every sprig is offered it\n", id)
	return nil
}

func printHints(k store.Skill) {
	for _, h := range agent.SkillHints(k) {
		fmt.Printf("  suggestion: %s\n", h)
	}
}

// skillCheck validates skill files without saving them, so people can
// write one by hand and see what's wrong before importing it.
func skillCheck(files []string) error {
	if len(files) == 0 {
		return errors.New("usage: thicket skill check FILE...")
	}
	bad := 0
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		k, err := agent.ParseSkill(b)
		if err != nil {
			fmt.Printf("%s: %v\n", f, err)
			bad++
			continue
		}
		fmt.Printf("%s: ok (%s)\n", f, k.ID)
		printHints(k)
	}
	if bad > 0 {
		return fmt.Errorf("%d of %d files can't be imported", bad, len(files))
	}
	return nil
}

// skillStarter lists the bundled example skills, or adds the named ones.
// A skill whose id is already in the library is left alone, so your own
// edits are never replaced by the example.
func skillStarter(ctx context.Context, db *store.Store, ids []string) error {
	pack, err := agent.StarterSkills()
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		fmt.Println("Starter skills (add with: thicket skill starter ID... or all):")
		for _, k := range pack {
			fmt.Printf("  %-20s %s\n", k.ID, k.Description)
		}
		return nil
	}
	want := map[string]bool{}
	for _, id := range ids {
		want[strings.ToLower(id)] = true
	}
	all := want["all"]
	known := map[string]bool{"all": true}
	for _, k := range pack {
		known[k.ID] = true
	}
	for id := range want {
		if !known[id] {
			return fmt.Errorf("no starter skill %q; run `thicket skill starter` to list them", id)
		}
	}
	for _, k := range pack {
		if !all && !want[k.ID] {
			continue
		}
		if cur, err := db.SkillByID(ctx, k.ID, 0); err == nil {
			fmt.Printf("skipped %s: you already have it (version %d)\n", k.ID, cur.Version)
			continue
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		k.Author, k.Note = "import", "starter pack"
		v, err := db.SaveSkill(ctx, k, 0)
		if err != nil {
			return fmt.Errorf("%s: %w", k.ID, err)
		}
		fmt.Printf("added %s (version %d)\n", k.ID, v)
	}
	return nil
}
