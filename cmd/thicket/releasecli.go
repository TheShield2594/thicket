package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	thicket "github.com/theshield2594/thicket"
	"github.com/theshield2594/thicket/internal/backup"
	"github.com/theshield2594/thicket/internal/changelog"
	"github.com/theshield2594/thicket/internal/settings"
	"github.com/theshield2594/thicket/internal/update"
)

// changelogCmd prints release notes from the CHANGELOG.md this binary was
// built with. With a version it prints that release's section and fails
// if there is none, which is how the release workflow refuses to publish
// a tag without notes.
func changelogCmd(args []string, out io.Writer) error {
	switch len(args) {
	case 0:
		secs := changelog.Between(thicket.Changelog, "", "")
		if version != "dev" && changelog.IsRelease(version) {
			if secs = changelog.Between(thicket.Changelog, "", version); len(secs) > 1 {
				secs = secs[:1]
			}
		}
		for _, s := range secs {
			fmt.Fprintf(out, "## %s\n\n%s\n\n", s.Heading, s.Body)
		}
		if len(secs) == 0 {
			fmt.Fprintln(out, "Nothing listed for this build.")
		}
		return nil
	case 1:
		s, err := changelog.Notes(thicket.Changelog, args[0])
		if err != nil {
			return err
		}
		fmt.Fprintln(out, s.Body)
		return nil
	}
	return errors.New("usage: thicket changelog [VERSION]")
}

// settingsCmd lists settings, or changes one, without the server: the
// same settings Juni changes in chat.
func settingsCmd(args []string, out io.Writer) error {
	cfg := loadConfig()
	db, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	st := settings.New(db, cfg)
	ctx := context.Background()
	switch {
	case len(args) == 0 || (len(args) == 1 && args[0] == "list"):
		for _, line := range st.Describe(ctx) {
			fmt.Fprintln(out, line)
		}
		return nil
	case args[0] == "set" && (len(args) == 2 || len(args) == 3):
		value := ""
		if len(args) == 3 {
			value = args[2]
		}
		if strings.HasPrefix(args[1], "budget.character.") {
			return errors.New("per-sprig caps are set in chat (ask Juni), where the sprig's id can be checked")
		}
		if err := st.Set(ctx, args[1], value); err != nil {
			return err
		}
		fmt.Fprintf(out, "%s = %s\n", args[1], st.String(ctx, args[1]))
		if backup.ServerRunning(cfg.DataDir) {
			fmt.Fprintln(out, "The running server picks this up by itself.")
		}
		return nil
	}
	return errors.New("usage: thicket settings [list] | settings set NAME [VALUE]")
}

// updateCmd moves the data directory to this binary's version, or lists
// past updates.
func updateCmd(args []string, in io.Reader, out io.Writer) error {
	cfg := loadConfig()
	if len(args) == 1 && args[0] == "history" {
		hist, err := update.History(cfg.DataDir)
		if err != nil {
			return err
		}
		if len(hist) == 0 {
			fmt.Fprintln(out, "No updates recorded yet.")
		}
		for _, r := range hist {
			from := r.From
			if from == "" {
				from = "(earlier)"
			}
			fmt.Fprintf(out, "%s  %s -> %s  schema v%d -> v%d  %s %s\n", r.At.Format("2006-01-02 15:04"), from, r.To, r.FromSchema, r.ToSchema, r.Outcome, r.Detail)
		}
		return nil
	}
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "don't ask before updating")
	to := fs.String("to", "", "the version you mean to end up on; refused if this isn't that version")
	force := fs.Bool("force", false, "allow moving to an older version that has the same schema")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return errors.New("usage: thicket update [--yes] [--to VERSION] [--force] | thicket update history")
	}
	if *to != "" && changelog.Normalize(*to) != changelog.Normalize(version) {
		return fmt.Errorf("this is Thicket %s, not %s. An update is run from the version you're moving to: set THICKET_VERSION=%s, `docker compose pull`, then `docker compose run --rm thicket update`", version, *to, changelog.Normalize(*to))
	}
	u := &update.Updater{Cfg: cfg, Version: version, Changelog: thicket.Changelog, Out: out, Force: *force}
	if !*yes {
		rd := bufio.NewReader(in)
		u.Confirm = func(q string) bool {
			fmt.Fprint(out, q+" [y/N] ")
			line, _ := rd.ReadString('\n')
			ans := strings.ToLower(strings.TrimSpace(line))
			return ans == "y" || ans == "yes"
		}
	}
	_, err = u.Run(context.Background())
	return err
}
