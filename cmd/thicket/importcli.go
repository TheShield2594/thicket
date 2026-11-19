package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/charimport"
	"github.com/theshield2594/thicket/internal/config"
	"github.com/theshield2594/thicket/internal/store"
)

const importUsage = `usage: thicket character import FILE|FOLDER [--from FORMAT] [--id ID] [--name NAME] [--memories] [--user ID] [--yes]

FORMAT is auto (default), thicket, card, openclaw, hermes, grok or text:
  card      a character card (chara_card v1/v2/v3) as .json or .png
  openclaw  an OpenClaw workspace (~/.openclaw/workspace)
  hermes    a Hermes Agent home (~/.hermes)
  grok      a Grok companion persona or custom instructions, pasted into a text file
  text      any system prompt or instruction file

You see the sprig before anything is written. Imported sprigs never get
tools beyond "schedule" and are never the host.`

func characterImportCmd(cfg *config.Config, args []string, in io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("character import", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	from := fs.String("from", "auto", "")
	id := fs.String("id", "", "")
	name := fs.String("name", "", "")
	withMem := fs.Bool("memories", false, "")
	userID := fs.Int64("user", 0, "")
	yes := fs.Bool("yes", false, "")
	pos, err := parseFlags(fs, args)
	if err != nil || len(pos) != 1 {
		return errors.New(importUsage)
	}
	d, err := charimport.Load(pos[0], charimport.Options{From: *from, ID: *id, Name: *name})
	if err != nil {
		return err
	}
	c := d.Char
	if _, err := os.Stat(filepath.Join(cfg.CharactersDir, c.ID+".md")); err == nil {
		return fmt.Errorf("a sprig with id %q already exists; pass --id to pick another", c.ID)
	}

	md, err := character.Marshal(c)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Importing %s from %s. This is the file it will get:\n\n", c.DisplayName(), d.Source)
	fmt.Fprintf(out, "--- characters/%s.md\n%s\n", c.ID, md)
	if d.Avatar != nil {
		fmt.Fprintf(out, "Avatar: characters/%s%s (%d KB)\n", c.ID, d.AvatarExt, (len(d.Avatar)+1023)/1024)
	}
	if len(d.Memories) > 0 {
		if *withMem {
			fmt.Fprintf(out, "Memories (%d), saved as yours so automatic review never changes them:\n", len(d.Memories))
			for _, m := range d.Memories {
				scope := "shared"
				if m.Personal {
					scope = "just " + c.DisplayName()
				}
				fmt.Fprintf(out, "  [%s] %s\n", scope, m.Content)
			}
		} else {
			fmt.Fprintf(out, "%s found; pass --memories to bring them too.\n", plural(len(d.Memories), "memory", "memories"))
		}
	}
	if len(d.Stripped) > 0 {
		fmt.Fprintln(out, "Removed for safety (grant in chat if you want them):", strings.Join(d.Stripped, ", "))
	}
	for _, n := range d.Notes {
		fmt.Fprintln(out, "Note:", n)
	}
	fmt.Fprintln(out)

	if !*yes {
		fmt.Fprintf(out, "Create %s? [y/N] ", c.DisplayName())
		line, _ := bufio.NewReader(in).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			fmt.Fprintln(out, "Nothing was written.")
			return nil
		}
	}

	db, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	ctx := context.Background()
	var owner *store.User
	if *withMem && len(d.Memories) > 0 {
		if *userID != 0 {
			owner, err = db.UserByID(ctx, *userID)
		} else {
			owner, err = db.Owner(ctx)
		}
		if errors.Is(err, store.ErrNotFound) {
			return errors.New("there's no user to give the memories to yet; finish setup first, or pass --user ID")
		}
		if err != nil {
			return err
		}
	}
	if err := charimport.Write(d, cfg.CharactersDir); err != nil {
		return err
	}
	undo := func(cause error) error {
		charimport.Remove(d, cfg.CharactersDir)
		_ = db.SetToolLevel(ctx, c.ID, store.AnyTool, "")
		return fmt.Errorf("%w (nothing was imported)", cause)
	}
	// Like sprigs Juni brings on, an import starts with every tool at draft.
	if err := db.SetToolLevel(ctx, c.ID, store.AnyTool, store.LevelDraft); err != nil {
		return undo(err)
	}
	if owner != nil {
		var changes []store.MemoryChange
		for _, m := range d.Memories {
			mem := store.Memory{UserID: owner.ID, Kind: m.Kind, Content: m.Content, Source: "user", Locked: true, Origin: store.OriginImport}
			if m.Personal {
				mem.Character = c.ID
			}
			changes = append(changes, store.MemoryChange{Action: "add", Memory: mem})
		}
		if _, err := db.ApplyMemoryChanges(ctx, 0, changes); err != nil {
			return undo(err)
		}
	}
	fmt.Fprintf(out, "Imported %s (%s). A running Thicket picks it up within 10 seconds.\n", c.DisplayName(), c.ID)
	if owner != nil {
		fmt.Fprintf(out, "Saved %s for %s.\n", plural(len(d.Memories), "memory", "memories"), userLabel(owner))
	}
	fmt.Fprintln(out, "Tools start at draft. Ask Juni to adjust anything that doesn't feel right.")
	return nil
}

func userLabel(u *store.User) string {
	if u.Name != "" {
		return u.Name
	}
	return fmt.Sprintf("user %d", u.ID)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
