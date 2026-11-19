package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/theshield2594/thicket/internal/config"
	"github.com/theshield2594/thicket/internal/store"
)

// `thicket pause` is the kill switch at the server. It writes the pause
// straight into the database, so it works without an API token and while
// the server is busy or down; a running server picks it up within a
// second and stops what's in flight. It acts as the server's operator,
// who may pause everything.
func pauseCmd(cfg *config.Config, args []string, out io.Writer, on bool) error {
	name := "pause"
	if !on {
		name = "resume"
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	sprig := fs.String("sprig", "", "")
	reason := fs.String("reason", "", "")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	db, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	ctx := context.Background()
	if on && len(pos) == 1 && pos[0] == "status" {
		return printPauses(ctx, cfg, db, out)
	}
	if len(pos) != 0 {
		return fmt.Errorf("usage: thicket %s [--sprig ID]%s", name, map[bool]string{true: " [--reason TEXT] | thicket pause status", false: ""}[on])
	}
	scope, label := store.PauseAll, "everything"
	if *sprig != "" {
		c, ok := findSprigFile(cfg, db, *sprig)
		if !ok {
			return fmt.Errorf("no sprig %q", *sprig)
		}
		scope, label = c.ID, c.DisplayName()
	}
	how := "from the command line"
	if *reason != "" {
		how += ": " + *reason
	}
	var changed bool
	if on {
		changed, err = db.SetPause(ctx, scope, 0, how)
	} else {
		changed, err = db.ClearPause(ctx, scope)
	}
	if err != nil {
		return err
	}
	char := ""
	if scope != store.PauseAll {
		char = scope
	}
	switch {
	case on && changed:
		_ = db.AddTrail(ctx, store.TrailEntry{Character: char, Kind: store.TrailPause, Summary: "paused " + label, Detail: how})
		fmt.Fprintf(out, "Paused %s. A running Thicket stops what it covers within a second; nothing runs until `thicket resume`.\n", label)
	case on:
		fmt.Fprintf(out, "%s was already paused.\n", label)
	case changed:
		_ = db.AddTrail(ctx, store.TrailEntry{Character: char, Kind: store.TrailPause, Summary: "resumed " + label, Detail: how})
		fmt.Fprintf(out, "Resumed %s.\n", label)
	default:
		fmt.Fprintf(out, "%s wasn't paused.\n", label)
	}
	if !on && scope != store.PauseAll {
		if _, all, _ := db.PausedFor(ctx, ""); all {
			fmt.Fprintln(out, "Everything is still paused, though: `thicket resume` lifts that.")
		}
	}
	return nil
}

func printPauses(ctx context.Context, cfg *config.Config, db *store.Store, out io.Writer) error {
	ps, err := db.Pauses(ctx)
	if err != nil {
		return err
	}
	if len(ps) == 0 {
		fmt.Fprintln(out, "Nothing is paused.")
		return nil
	}
	reg := loadRegistry(cfg, db)
	for _, p := range ps {
		label := "everything"
		if p.Scope != store.PauseAll {
			label = p.Scope
			if c, ok := reg.Get(p.Scope); ok {
				label = c.DisplayName()
			}
		}
		fmt.Fprintf(out, "paused  %-20s since %s  %s\n", label, p.PausedAt.Local().Format("2006-01-02 15:04"), p.Reason)
	}
	return nil
}
