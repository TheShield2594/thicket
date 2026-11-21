package plugin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/theshield2594/thicket/internal/browser"
)

// browserCheck is the browser plugin's doctor check: Chromium starts, and
// its egress proxy refuses a host off the allowlist and private
// addresses on it.
func browserCheck(ctx context.Context, url, token string) (string, error) {
	cl := &browser.Client{URL: url, Token: func(context.Context) (string, error) { return token, nil }}
	rep, err := cl.Doctor(ctx)
	if err != nil {
		return "", err
	}
	if len(rep.Checks) == 0 {
		return "", errors.New("the plugin reported no checks")
	}
	var bad, ok []string
	for _, c := range rep.Checks {
		if c.OK {
			ok = append(ok, c.Detail)
		} else {
			bad = append(bad, c.Name+": "+c.Detail)
		}
	}
	note := fmt.Sprintf("%d of %d browsers open", len(rep.Open), rep.Max)
	if len(bad) > 0 {
		return note, errors.New(strings.Join(bad, "; "))
	}
	return note + "; " + strings.Join(ok, "; "), nil
}
