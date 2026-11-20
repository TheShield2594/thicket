package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/store"
)

// Presence: where proactive messages (check-ins, job output, reminders,
// notices) reach a person.
//
//   - A person can choose ("reach me on Telegram"), for every sprig or for
//     one ("Pip always writes on Discord"): set_reach, kept in reach_prefs.
//   - Otherwise it's the chat app they last wrote from, preferring one on
//     their phone (Telegram, Discord) over the web app when they have both.
//   - If sending there fails, each of their other linked accounts is tried
//     before giving up (callers then keep the message in the inbox).
//   - Someone only on the web app is told once, by the host, that a chat
//     app would bring these to their phone.

// phoneApps are the chat apps that reach someone on their phone.
var phoneApps = map[string]bool{"discord": true, "telegram": true, "matrix": true}

// appName is how a chat app is named to people.
func appName(ch string) string {
	switch ch {
	case "web":
		return "the web app"
	case "":
		return ""
	}
	return strings.ToUpper(ch[:1]) + ch[1:]
}

// reachOrder lists the person's accounts in the order a proactive message
// from c should try them. c may be nil (no sprig in particular).
func (e *Engine) reachOrder(ctx context.Context, u *store.User, c *character.Character) ([]store.Identity, error) {
	ids, err := e.DB.Identities(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	last := store.Identity{Channel: u.Channel, ExternalID: u.ExternalID}
	isLast := func(i store.Identity) bool { return i.Channel == last.Channel && i.ExternalID == last.ExternalID }
	found := false
	for _, i := range ids {
		found = found || isLast(i)
	}
	if !found && last.Channel != "" {
		ids = append(ids, last) // an account from before identities were kept
	}
	var usable []store.Identity
	for _, i := range ids {
		if i.Channel != "api" && e.channelFor(i.Channel) != nil {
			usable = append(usable, i)
		}
	}
	// Most recently used first; the last-used account wins a tie.
	sort.SliceStable(usable, func(a, b int) bool {
		if isLast(usable[a]) != isLast(usable[b]) {
			return isLast(usable[a])
		}
		return usable[a].LastSeenAt.After(usable[b].LastSeenAt)
	})
	prefs, err := e.DB.ReachPrefs(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	want := prefs[""]
	if c != nil && prefs[c.ID] != "" {
		want = prefs[c.ID]
	}
	var order []store.Identity
	taken := map[store.Identity]bool{}
	add := func(ok func(store.Identity) bool) {
		for _, i := range usable {
			key := store.Identity{Channel: i.Channel, ExternalID: i.ExternalID}
			if !taken[key] && ok(i) {
				taken[key] = true
				order = append(order, i)
			}
		}
	}
	add(func(i store.Identity) bool { return want != "" && i.Channel == want })
	add(func(i store.Identity) bool { return isLast(i) && phoneApps[i.Channel] })
	add(func(i store.Identity) bool { return phoneApps[i.Channel] })
	add(func(i store.Identity) bool { return true })
	return order, nil
}

// directTo posts text as c in the person's private chat, trying each
// place they can be reached in turn. It returns the chat it went to.
func (e *Engine) directTo(ctx context.Context, u *store.User, c *character.Character, text string) (string, error) {
	order, err := e.reachOrder(ctx, u, c)
	if err != nil {
		return "", err
	}
	if len(order) == 0 {
		return "", fmt.Errorf("no chat app to reach user %d on (their apps: %s)", u.ID, orNone(u.Channel))
	}
	var errs []error
	for _, i := range order {
		ch := e.channelFor(i.Channel)
		if ch == nil {
			continue
		}
		chatID, err := ch.SendDirect(ctx, i.ExternalID, e.persona(c), text)
		if err == nil {
			if len(errs) > 0 {
				e.logActivity(ctx, "warn", "system", c.ID, u.ID, "delivered on "+appName(i.Channel)+" instead", errors.Join(errs...).Error())
			}
			return i.Channel + ":" + chatID, nil
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		e.Log.Warn("proactive delivery failed, trying the next app", "user", u.ID, "channel", i.Channel, "err", err)
		errs = append(errs, fmt.Errorf("%s: %w", i.Channel, err))
	}
	return "", errors.Join(errs...)
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// suggestChatApp tells someone who only has the web app, once, that a
// chat app would bring messages like this to their phone, if the
// household has one connected.
func (e *Engine) suggestChatApp(ctx context.Context, u *store.User, chatRef string) {
	if !strings.HasPrefix(chatRef, "web:") || u.IsKid() {
		return
	}
	var apps []string
	for name := range e.Channels() {
		if phoneApps[name] {
			apps = append(apps, appName(name))
		}
	}
	if len(apps) == 0 {
		return
	}
	ids, err := e.DB.Identities(ctx, u.ID)
	if err != nil {
		return
	}
	for _, i := range ids {
		if phoneApps[i.Channel] {
			return
		}
	}
	if ok, err := e.DB.ClaimNotice(ctx, fmt.Sprintf("suggest-chat-app:%d", u.ID), 100*365*24*time.Hour); err != nil || !ok {
		return
	}
	host, ok := e.Chars.Host()
	if !ok {
		return
	}
	sort.Strings(apps)
	msg := fmt.Sprintf("By the way: messages like that one wait here in the web app until you open it. "+
		"If you'd rather get them on your phone, link %s: just ask me to link it, and I'll write there from then on.", strings.Join(apps, " or "))
	if _, err := e.directTo(ctx, u, host, msg); err != nil {
		e.Log.Debug("suggest a chat app", "err", err)
	}
}

func (e *Engine) toolSetReach(ctx context.Context, t *turn, args json.RawMessage) (string, error) {
	a, err := decode[struct {
		App   string `json:"app"`
		Sprig string `json:"sprig"`
	}](args)
	if err != nil {
		return "", err
	}
	app := strings.ToLower(strings.TrimSpace(a.App))
	switch app {
	case "auto", "default", "":
		app = ""
	case "web app", "web":
		app = "web"
	case "discord", "telegram", "matrix":
	default:
		return "", fmt.Errorf("app is telegram, discord, matrix, web or auto, not %q", a.App)
	}
	sprig := ""
	who := "every sprig"
	if s := strings.TrimSpace(a.Sprig); s != "" {
		c, ok := e.Chars.Get(strings.ToLower(s))
		if !ok {
			c, ok = e.Chars.FindByName(s)
		}
		if !ok || !c.UsableBy(t.user) {
			return "", fmt.Errorf("there's no sprig %q they talk to", s)
		}
		sprig, who = c.ID, c.DisplayName()
	}
	if app != "" {
		if e.channelFor(app) == nil {
			return "", fmt.Errorf("%s isn't connected to this Thicket", appName(app))
		}
		ids, err := e.DB.Identities(ctx, t.user.ID)
		if err != nil {
			return "", err
		}
		linked := false
		for _, i := range ids {
			linked = linked || i.Channel == app
		}
		if !linked {
			return "", fmt.Errorf("they haven't linked %s yet: offer link_account (they send a code from %s), then set this", appName(app), appName(app))
		}
	}
	if err := e.DB.SetReach(ctx, t.user.ID, sprig, app); err != nil {
		return "", err
	}
	e.logActivity(ctx, "info", "system", t.char.ID, t.user.ID, "proactive messages from "+who+" now go to "+orDefaultReach(app), "")
	return fmt.Sprintf("Done: messages from %s now reach them on %s. If that ever fails, their other linked apps are tried before anything waits in the inbox.", who, orDefaultReach(app)), nil
}

func orDefaultReach(app string) string {
	if app == "" {
		return "the app they last used (a phone app before the web app)"
	}
	return appName(app)
}
