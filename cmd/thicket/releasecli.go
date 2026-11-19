package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	thicket "github.com/theshield2594/thicket"
	"github.com/theshield2594/thicket/internal/backup"
	"github.com/theshield2594/thicket/internal/changelog"
	"github.com/theshield2594/thicket/internal/settings"
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
