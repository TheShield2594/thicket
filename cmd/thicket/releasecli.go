package main

import (
	"errors"
	"fmt"
	"io"

	thicket "github.com/theshield2594/thicket"
	"github.com/theshield2594/thicket/internal/changelog"
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
