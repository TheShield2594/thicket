package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/theshield2594/thicket/internal/backup"
	"github.com/theshield2594/thicket/internal/browser"
	"github.com/theshield2594/thicket/internal/config"
	"github.com/theshield2594/thicket/internal/plugin"
)

// browserProfiles fetches every sprig's sealed browser profile from the
// browser plugin, for `thicket backup --browser-profiles`. They stay
// sealed with keys from Thicket's secret store. A profile whose browser
// is open is taken as it was when it last closed (said on stderr).
func browserProfiles(ctx context.Context, cfg *config.Config) ([]backup.BrowserProfile, error) {
	ep := plugin.Browser.Resolve(cfg.LookupPluginEnv, cfg.DataDir)
	if ep.URL == "" {
		return nil, errors.New("the browser plugin is turned off, so there are no browser profiles to back up")
	}
	b, err := os.ReadFile(ep.TokenFile)
	tok := strings.TrimSpace(string(b))
	if err != nil || tok == "" {
		return nil, fmt.Errorf("no token shared with the browser plugin at %s (run this where Thicket runs: docker compose exec thicket thicket backup --browser-profiles)", ep.TokenFile)
	}
	cl := &browser.Client{URL: ep.URL, HTTP: &http.Client{Timeout: 5 * time.Minute},
		Token: func(context.Context) (string, error) { return tok, nil }}
	list, err := cl.Profiles(ctx)
	if err != nil {
		return nil, fmt.Errorf("the browser plugin at %s didn't list its profiles: %w", ep.URL, err)
	}
	var out []backup.BrowserProfile
	for _, p := range list {
		sealed, err := cl.Profile(ctx, p.Sprig)
		if errors.Is(err, browser.ErrNotFound) {
			continue // deleted meanwhile
		}
		if err != nil {
			return nil, fmt.Errorf("%s's browser profile: %w", p.Sprig, err)
		}
		if p.Open {
			fmt.Fprintf(os.Stderr, "note: %s's browser is open; its profile is backed up as it was when it last closed\n", p.Sprig)
		}
		out = append(out, backup.BrowserProfile{Sprig: p.Sprig, Sealed: sealed})
	}
	return out, nil
}
