package plugin

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/theshield2594/thicket/internal/mail"
)

// mailCheck is the mail plugin's doctor check: for each sprig address,
// the server answers, the login works, new mail arrives (push, IDLE or
// polling) and sending works; missing SPF, DKIM or DMARC records for the
// sending domain are advice, not a failure.
func mailCheck(ctx context.Context, url, token string) (string, error) {
	cl := &mail.Client{URL: url, Token: func(context.Context) (string, error) { return token, nil }}
	rep, err := cl.Doctor(ctx)
	if err != nil {
		return "", err
	}
	if len(rep.Accounts) == 0 {
		return "no sprig addresses yet", nil
	}
	var bad, warn, ok []string
	for _, a := range rep.Accounts {
		var problems []string
		for _, c := range a.Checks {
			switch {
			case c.OK && c.Warn:
				warn = append(warn, fmt.Sprintf("%s: %s", a.Address, c.Detail))
			case c.OK:
			case c.Warn:
				warn = append(warn, fmt.Sprintf("%s: %s", a.Address, c.Detail))
			default:
				problems = append(problems, c.Name+": "+c.Detail)
			}
		}
		if len(problems) > 0 {
			bad = append(bad, a.Address+" ("+strings.Join(problems, "; ")+")")
		} else {
			ok = append(ok, a.Address)
		}
	}
	note := ""
	if len(ok) > 0 {
		note = "working: " + strings.Join(ok, ", ")
	}
	if len(warn) > 0 {
		note = strings.TrimPrefix(note+"; advice: "+strings.Join(warn, "; "), "; ")
	}
	if len(bad) > 0 {
		return note, errors.New(strings.Join(bad, ", "))
	}
	return note, nil
}
