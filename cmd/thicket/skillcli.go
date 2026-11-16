package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/theshield2594/thicket/internal/agent"
	"github.com/theshield2594/thicket/internal/store"
)

func skillCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: thicket skill list | export ID [-o FILE] | import FILE")
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
			fmt.Printf("%-24s v%-3d %s: %s\n", k.ID, k.Version, k.Name, k.Description)
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
		return nil
	}
	return fmt.Errorf("unknown skill command %q", args[0])
}
