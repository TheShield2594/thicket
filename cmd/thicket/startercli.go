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
	"strconv"
	"strings"

	"github.com/theshield2594/thicket/internal/agent"
	"github.com/theshield2594/thicket/internal/config"
	"github.com/theshield2594/thicket/internal/sprigpkg"
	"github.com/theshield2594/thicket/internal/store"
)

const sprigStarterUsage = `usage: thicket sprig starter [ID...] [--tier solo|family|kid] [--kid USER]... [--parent USER]... [--user USER] [--id ID] [--name NAME] [--yes]
       thicket sprig starter --export DIR

With no ID, lists the starter sprigs. With IDs, shows everything each one
will get and adds it once you say yes. Suggested routines arrive paused.

Kid starters need --kid (a user id or name of a kid in the household) and
are set up by --parent (default: the owner), who can read their chats.
--user is who a solo sprig belongs to and who runs routines (default: the
owner). --export writes each starter as a .sprig package.`

func sprigCmd(cfg *config.Config, args []string, in io.Reader, out io.Writer) error {
	if len(args) == 0 || args[0] != "starter" {
		return errors.New(sprigStarterUsage)
	}
	return sprigStarterCmd(cfg, args[1:], in, out)
}

func sprigStarterCmd(cfg *config.Config, args []string, in io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("sprig starter", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	tier := fs.String("tier", "", "")
	var kids, parents multiFlag
	fs.Var(&kids, "kid", "")
	fs.Var(&parents, "parent", "")
	userArg := fs.String("user", "", "")
	id := fs.String("id", "", "")
	name := fs.String("name", "", "")
	yes := fs.Bool("yes", false, "")
	export := fs.String("export", "", "")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return errors.New(sprigStarterUsage)
	}
	all, err := agent.StarterSprigs()
	if err != nil {
		return err
	}
	if *export != "" {
		return exportStarters(*export, pos, out)
	}
	if len(pos) == 0 {
		fmt.Fprintln(out, "Starter sprigs (add with: thicket sprig starter ID):")
		for _, s := range all {
			fmt.Fprintf(out, "  %-16s %-7s %s, %s. %s\n", s.ID, s.Char.DisplayName(), s.TierLabel(), fmt.Sprintf("$%.2f a day", s.DailyUSD), s.Summary)
		}
		return nil
	}
	if len(pos) > 1 && (*id != "" || *name != "") {
		return errors.New("--id and --name are for adding one starter at a time")
	}
	db, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	ctx := context.Background()
	users, err := db.ListUsers(ctx)
	if err != nil {
		return err
	}
	findUser := func(s string) (*store.User, error) {
		for i, u := range users {
			if strconv.FormatInt(u.ID, 10) == s || strings.EqualFold(u.Name, s) {
				return &users[i], nil
			}
		}
		return nil, fmt.Errorf("there's no user %q (see thicket user list)", s)
	}
	var owner *store.User
	if *userArg != "" {
		if owner, err = findUser(*userArg); err != nil {
			return err
		}
	} else if owner, err = db.Owner(ctx); errors.Is(err, store.ErrNotFound) {
		owner = nil
	} else if err != nil {
		return err
	}
	o := agent.StarterOptions{Tier: strings.ToLower(*tier), ID: *id, Name: *name, CanWriteSkills: true}
	if owner != nil {
		o.Owner = owner.ID
	}
	for _, k := range kids {
		u, err := findUser(k)
		if err != nil {
			return err
		}
		o.Kids = append(o.Kids, u.ID)
	}
	for _, p := range parents {
		u, err := findUser(p)
		if err != nil {
			return err
		}
		o.Parents = append(o.Parents, u.ID)
	}
	if len(o.Parents) == 0 && owner != nil && !owner.IsKid() {
		o.Parents = []int64{owner.ID}
	}
	reg := loadRegistry(cfg, db)
	taken := func(s string) bool { _, ok := reg.FindByName(s); return ok }
	stdin := bufio.NewReader(in)
	for _, want := range pos {
		s, err := agent.FindStarterSprig(want)
		if err != nil {
			return err
		}
		so := o
		if so.Tier == "" {
			so.Tier = s.Tiers[0]
		}
		if so.Tier == "kid" && len(so.Kids) == 0 {
			return fmt.Errorf("%s is a kid sprig: pass --kid with the kid it's for (a kid in thicket user list)", s.ID)
		}
		if so.Tier != "kid" {
			so.Kids, so.Parents = nil, nil
		}
		st, err := agent.PrepareStarter(ctx, db, cfg.CharactersDir, taken, s, so)
		if err != nil {
			var c *sprigpkg.Clash
			if errors.As(err, &c) && c.Suggest != "" {
				return fmt.Errorf("%s: %w: pass --%s %q", s.ID, err, c.What, c.Suggest)
			}
			return fmt.Errorf("%s: %w", s.ID, err)
		}
		preview, err := st.Preview(true)
		if err != nil {
			return err
		}
		c := st.Pkg.Char
		fmt.Fprintf(out, "Adding %s (the %s starter). This is everything it will get:\n\n%s\n", c.DisplayName(), s.ID, preview)
		if !*yes {
			fmt.Fprintf(out, "Add %s? [y/N] ", c.DisplayName())
			line, _ := stdin.ReadString('\n')
			if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
				fmt.Fprintln(out, "Nothing was written.")
				continue
			}
		}
		res, err := agent.InstallStarter(ctx, db, cfg.CharactersDir, st)
		if err != nil {
			return fmt.Errorf("%s: %w", s.ID, err)
		}
		reg = loadRegistry(cfg, db) // the next one sees this one's name
		fmt.Fprintf(out, "Added %s (%s). A running Thicket picks it up within 10 seconds.\n", c.DisplayName(), c.ID)
		if len(res.Routines) > 0 {
			fmt.Fprintf(out, "Suggested routines, paused: %d (turn them on in chat or on the dashboard).\n", len(res.Routines))
		}
		if len(res.Skills) > 0 {
			fmt.Fprintln(out, "Skills:", strings.Join(res.Skills, ", "))
		}
	}
	return nil
}

// exportStarters writes starters (all, or the named ones) as .sprig files.
func exportStarters(dir string, ids []string, out io.Writer) error {
	all, err := agent.StarterSprigs()
	if err != nil {
		return err
	}
	pick := all
	if len(ids) > 0 {
		pick = nil
		for _, id := range ids {
			s, err := agent.FindStarterSprig(id)
			if err != nil {
				return err
			}
			pick = append(pick, s)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, s := range pick {
		path := filepath.Join(dir, s.ID+sprigpkg.Ext)
		if err := writeNew(path, s.Package); err != nil {
			return err
		}
		fmt.Fprintf(out, "wrote %s (%s)\n", path, s.Char.DisplayName())
	}
	return nil
}
