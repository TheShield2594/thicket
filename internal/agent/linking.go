package agent

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/theshield2594/thicket/internal/channel"
	"github.com/theshield2594/thicket/internal/store"
)

// Linking lets one person use Thicket from several chat apps with one
// profile: they ask for a code on an app Thicket knows them on, and send
// it from the new one. Codes are single-use, expire after 15 minutes,
// live only in memory, and are sent by code (never seen by the model or
// kept in the history), like webhook URLs.

const (
	linkCodeTTL      = 15 * time.Minute
	linkFailsPerHour = 5
	linkAlphabet     = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // no 0/O, 1/I
)

var linkCodeRe = regexp.MustCompile("(?i)^\\s*`?link-([a-z0-9]{4})-?([a-z0-9]{4})`?\\s*$")

type linkCode struct {
	userID  int64
	expires time.Time
}

func newLinkCode() (string, error) {
	var b strings.Builder
	b.WriteString("LINK-")
	for i := 0; i < 8; i++ {
		if i == 4 {
			b.WriteByte('-')
		}
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(linkAlphabet))))
		if err != nil {
			return "", err
		}
		b.WriteByte(linkAlphabet[n.Int64()])
	}
	return b.String(), nil
}

func normaliseCode(s string) string {
	m := linkCodeRe.FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	return "LINK-" + strings.ToUpper(m[1]) + "-" + strings.ToUpper(m[2])
}

func (e *Engine) toolLinkAccount(ctx context.Context, t *turn, _ json.RawMessage) (string, error) {
	code, err := newLinkCode()
	if err != nil {
		return "", err
	}
	e.linkMu.Lock()
	if e.links == nil {
		e.links = map[string]linkCode{}
	}
	now := e.now()
	for c, l := range e.links { // one live code per user; drop stale ones
		if l.userID == t.user.ID || now.After(l.expires) {
			delete(e.links, c)
		}
	}
	e.links[code] = linkCode{userID: t.user.ID, expires: now.Add(linkCodeTTL)}
	e.linkMu.Unlock()

	var apps []string
	for name := range e.channels {
		if name == "discord" || name == "telegram" {
			apps = append(apps, strings.ToUpper(name[:1])+name[1:])
		}
	}
	sort.Strings(apps)
	msg := fmt.Sprintf("🔑 Your link code: `%s`\nSend it to me in a private chat on the other app (%s) within 15 minutes, and that account joins this profile: same sprigs, same memories. "+
		"Don't share it: whoever sends it becomes you.", code, strings.Join(apps, " or "))
	ch := e.channels[t.user.Channel]
	if ch == nil {
		return "", fmt.Errorf("no channel %q", t.user.Channel)
	}
	if _, err := ch.SendDirect(ctx, t.user.ExternalID, e.persona(t.char), msg); err != nil {
		return "", fmt.Errorf("couldn't DM the code: %w", err)
	}
	e.logActivity(ctx, "info", "system", t.char.ID, t.user.ID, "link code issued", "")
	return "Thicket DMed the user a one-time link code (you can't see it). Tell them to send it to the bot in a private chat on the other app within 15 minutes.", nil
}

// redeemLinkCode handles a DM that is just a link code, from any account,
// known or not.
func (e *Engine) redeemLinkCode(ctx context.Context, in channel.Incoming) error {
	host, ok := e.Chars.Host()
	if !ok {
		return errors.New("no host character is loaded")
	}
	who := in.Channel + ":" + in.UserID
	now := e.now()
	e.linkMu.Lock()
	recent := e.linkFails[who][:0]
	for _, t := range e.linkFails[who] {
		if now.Sub(t) < time.Hour {
			recent = append(recent, t)
		}
	}
	if len(recent) >= linkFailsPerHour {
		e.linkMu.Unlock()
		return nil // stay quiet: guessing gets no feedback
	}
	code := normaliseCode(in.Text)
	l, found := e.links[code]
	if found {
		delete(e.links, code) // single use, even if it fails below
	}
	if !found || now.After(l.expires) {
		if e.linkFails == nil {
			e.linkFails = map[string][]time.Time{}
		}
		e.linkFails[who] = append(recent, now)
		e.linkMu.Unlock()
		return e.send(ctx, in.ChatRef(), host, "That link code isn't valid or has expired. Ask me for a new one on the app you already use.")
	}
	e.linkMu.Unlock()

	e.scrub(ctx, in)
	user, err := e.DB.UserByID(ctx, l.userID)
	if err != nil {
		return err
	}
	switch err := e.DB.LinkIdentity(ctx, user.ID, in.Channel, in.UserID); {
	case errors.Is(err, store.ErrIdentityTaken):
		return e.send(ctx, in.ChatRef(), host, "This account already has its own profile here (with its own memories or jobs, or as the owner), so I can't fold it into another one. Ask for a link code from this account instead and send it from the other app.")
	case err != nil:
		return err
	}
	_ = e.DB.UseIdentity(ctx, user.ID, in.Channel, in.UserID)
	e.logActivity(ctx, "info", "system", "", user.ID, "linked a "+in.Channel+" account", "")
	name := user.Name
	if name == "" {
		name = "you"
	}
	return e.send(ctx, in.ChatRef(), host, fmt.Sprintf("Linked! This account is now part of your profile, %s: same sprigs, same memories. I'll message you on whichever app you last wrote from.", name))
}
