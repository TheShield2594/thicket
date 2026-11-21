package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/theshield2594/thicket/internal/config"
	"github.com/theshield2594/thicket/internal/gallery"
	"github.com/theshield2594/thicket/internal/sprigpkg"
	"github.com/theshield2594/thicket/internal/websearch"
)

// galleryFetcher is what downloads from the gallery: public addresses
// only, as for web pages. Tests replace it.
var galleryFetcher = func(cfg *config.Config) *websearch.Fetcher {
	f := &websearch.Fetcher{}
	if cfg.WebProxy {
		f.Proxy = http.ProxyFromEnvironment
	}
	return f
}

// galleryClient is the configured gallery, or nil when it's off.
func galleryClient(cfg *config.Config) *gallery.Client {
	if cfg.GalleryURL == "" {
		return nil
	}
	return gallery.New(cfg.GalleryURL, cfg.DataDir, galleryFetcher(cfg))
}

// galleryIndexCmd: list, show, install and refresh.
func galleryIndexCmd(cfg *config.Config, args []string, in io.Reader, out io.Writer) error {
	c := galleryClient(cfg)
	if c == nil {
		return errors.New("no gallery is set up: set THICKET_GALLERY_URL to a gallery's index.json (see docs/gallery.md)")
	}
	ctx := context.Background()
	if args[0] == "refresh" {
		idx, err := c.Refresh(ctx)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Fetched the gallery index: %d sprigs, %d revoked author keys.\n", len(idx.Packages), len(idx.Revoked))
		return nil
	}
	idx, err := c.Index(ctx)
	if idx == nil {
		return err
	}
	if err != nil {
		fmt.Fprintln(out, "Note:", err)
	}
	db, err := openStore(cfg)
	if err != nil {
		return err
	}
	revoked, err := gallery.RevokedKeys(ctx, db, c)
	db.Close()
	if err != nil {
		return err
	}
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return errors.New("usage: thicket gallery list")
		}
		if len(idx.Packages) == 0 {
			fmt.Fprintln(out, "The gallery is empty.")
		}
		for _, p := range idx.Packages {
			mark := ""
			if slices.Contains(revoked, p.Author) {
				mark = " [author key revoked]"
			}
			fmt.Fprintf(out, "%-20s %s: %s (by %s, v%d)%s\n", p.Name, p.DisplayName, p.Description, idx.Authors[p.Author].Name, p.Latest().Version, mark)
		}
		return nil
	case "show":
		if len(args) != 2 {
			return errors.New("usage: thicket gallery show NAME")
		}
		p, ok := idx.Package(args[1])
		if !ok {
			return fmt.Errorf("the gallery has no sprig called %q (thicket gallery list shows them)", args[1])
		}
		a := idx.Authors[p.Author]
		fmt.Fprintf(out, "%s (%s)\n%s\n\nAuthor: %s", p.DisplayName, p.Name, p.Description, a.Name)
		if a.URL != "" {
			fmt.Fprintf(out, " %s", a.URL)
		}
		fmt.Fprintf(out, "\nAuthor key: %s\nFingerprint: %s\n", p.Author, a.Fingerprint)
		if slices.Contains(revoked, p.Author) {
			fmt.Fprintln(out, "This author key is revoked: it can't be installed.")
		}
		scopes := "no scopes"
		if len(p.Scopes) > 0 {
			scopes = strings.Join(p.Scopes, ", ")
		}
		fmt.Fprintf(out, "Asks for: %s (none are granted when you install it)\nVersions:\n", scopes)
		for _, v := range p.Versions {
			fmt.Fprintf(out, "  %d  %s  sha256 %s\n", v.Version, v.CreatedAt, v.SHA256)
		}
		return nil
	case "install":
		fs := flag.NewFlagSet("gallery install", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		version := fs.Int("version", 0, "")
		id := fs.String("id", "", "")
		name := fs.String("name", "", "")
		userID := fs.Int64("user", 0, "")
		yes := fs.Bool("yes", false, "")
		pos, err := parseFlags(fs, args[1:])
		if err != nil || len(pos) != 1 {
			return errors.New("usage: thicket gallery install NAME [--version N] [--id ID] [--name NAME] [--user ID] [--yes]")
		}
		f, err := c.Fetch(ctx, idx, pos[0], *version, revoked)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "Downloaded %s version %d; it matches the gallery index (sha256 %s).\n", f.Package.Name, f.Version.Version, f.Version.SHA256)
		return importGalleryCmd(cfg, f.File, f.Raw, *id, *name, *userID, *yes, in, out)
	}
	return errors.New(galleryUsage)
}

// galleryRepoCmd: verify, build-index and build-site, for a gallery's
// repository. They need no Thicket data, only the files.
func galleryRepoCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("gallery "+args[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	outDir := fs.String("o", "", "")
	pos, err := parseFlags(fs, args[1:])
	if err != nil || len(pos) != 1 || (*outDir != "" && args[0] != "build-site") {
		return fmt.Errorf("usage: thicket gallery %s DIR%s", args[0], map[bool]string{true: " [-o OUT]"}[args[0] == "build-site"])
	}
	dir := pos[0]
	switch args[0] {
	case "verify":
		problems, err := sprigpkg.VerifyRepo(dir)
		if err != nil {
			return err
		}
		for _, p := range problems {
			fmt.Fprintln(out, "✗", p)
		}
		if len(problems) > 0 {
			return fmt.Errorf("%s found: this change must not be merged", plural(len(problems), "problem", "problems"))
		}
		idx, _, _ := sprigpkg.ScanRepo(dir)
		fmt.Fprintf(out, "✓ %s and index.json check out.\n", plural(len(idx.Packages), "sprig", "sprigs"))
		return nil
	case "build-index":
		idx, problems, err := sprigpkg.ScanRepo(dir)
		if err != nil {
			return err
		}
		for _, p := range problems {
			fmt.Fprintln(out, "✗", p)
		}
		if len(problems) > 0 {
			return errors.New("index.json wasn't written: fix the problems above first")
		}
		b, err := idx.Marshal()
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, sprigpkg.IndexFile), b, 0o644); err != nil {
			return err
		}
		fmt.Fprintf(out, "Wrote %s: %s, %d authors, %d revoked keys.\n", filepath.Join(dir, sprigpkg.IndexFile), plural(len(idx.Packages), "sprig", "sprigs"), len(idx.Authors), len(idx.Revoked))
		return nil
	case "build-site":
		if *outDir == "" {
			*outDir = filepath.Join(dir, "site")
		}
		if err := gallery.BuildSite(dir, *outDir); err != nil {
			return err
		}
		fmt.Fprintf(out, "Wrote the site to %s. Publish that folder; THICKET_GALLERY_URL is its index.json.\n", *outDir)
		return nil
	}
	return errors.New(galleryUsage)
}
