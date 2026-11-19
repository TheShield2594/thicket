package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/theshield2594/thicket/internal/channel"
	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/store"
)

// Invite links: an admin asks Juni to add someone ("add my partner's
// sprig"), says how they'll chat, and gets one link to send them. The web
// app link opens the invite page; Telegram's opens a chat with the bot
// that starts with the invite; on Discord the person DMs the bot a join
// code. Whichever they use, Juni greets them (not the admin) as soon as
// they arrive. Like sign-in links, the link is posted by code after the
// reply: the model never sees it.

const inviteTTL = 7 * 24 * time.Hour

// joinCodeRe is a chat invite: "join-" and the invite's token, as sent to
// the bot (Telegram's /start sends it for them).
var joinCodeRe = regexp.MustCompile("^\\s*`?(?i:join)-([A-Za-z0-9_-]{43})`?\\s*$")

func (e *Engine) toolInvitePerson(ctx context.Context, t *turn, args json.RawMessage) (string, error) {
	a, err := decode[struct {
		Name     string `json:"name"`
		App      string `json:"app"`
		Role     string `json:"role"`
		OwnSprig bool   `json:"own_sprig"`
	}](args)
	if err != nil {
		return "", err
	}
	if !t.isDM {
		return "Invite links are private: ask for one in a DM (or the web app).", nil
	}
	name := strings.Join(strings.Fields(a.Name), " ")
	if name == "" || len(name) > 60 {
		return "", errors.New("name is required: ask what they're called (max 60 characters)")
	}
	role := a.Role
	if role == "" {
		role = store.RoleMember
	}
	if role != store.RoleMember && role != store.RoleAdmin && role != store.RoleKid {
		return "", errors.New("role must be member, admin or kid")
	}
	if role == store.RoleKid && a.OwnSprig {
		return "", errors.New("kids don't get a sprig of their own this way: invite them as a kid, then set up a kid sprig for them")
	}
	app := strings.ToLower(strings.TrimSpace(a.App))
	if app == "" {
		app = "web"
	}
	var bot channel.Bot
	switch app {
	case "web":
	case "telegram", "discord":
		ch := e.channelFor(app)
		if ch == nil {
			return "", fmt.Errorf("this Thicket isn't connected to %s yet: connect it with connect_chat_app, or invite them to the web app instead", app)
		}
		id, ok := ch.(channel.Identity)
		if ok {
			bot, ok = id.Bot()
		}
		if !ok {
			return "", fmt.Errorf("%s hasn't finished connecting: try again in a moment, or invite them to the web app instead", app)
		}
	default:
		return "", errors.New("app must be web, telegram or discord")
	}
	token, err := e.DB.CreateInvite(ctx, store.Invite{Role: role, Name: name, CreatedBy: t.user.ID, OwnSprig: a.OwnSprig}, inviteTTL)
	if err != nil {
		return "", err
	}
	var msg string
	switch app {
	case "web":
		msg = fmt.Sprintf("🔗 Invite for %s (works once, for 7 days). Send them this link; they choose a username and password and I'll say hello:\n%s/invite#%s",
			name, e.Cfg.WebBaseURL(), token)
	case "telegram":
		msg = fmt.Sprintf("🔗 Invite for %s (works once, for 7 days). Send them this link; it opens a chat with me on Telegram, and I'll say hello when they press Start:\n%s?start=join-%s",
			name, bot.URL, token)
	case "discord":
		msg = fmt.Sprintf("🔗 Invite for %s (works once, for 7 days). On Discord they need to share a server with me (%s). Send them this, to DM me as it is:\n`join-%s`",
			name, bot.Name, token)
	}
	msg += "\nDon't post it anywhere public: whoever uses it first becomes them."
	t.afterReply = append(t.afterReply, post{text: msg})
	what := role
	if a.OwnSprig {
		what += ", with their own private sprig"
	}
	e.logActivity(ctx, "info", "system", t.char.ID, t.user.ID, fmt.Sprintf("made a %s invite for %s (%s)", app, name, what), "")
	note := "The invite will be posted right after your reply (you can't see it): tell them to send it to " + name + " privately."
	if app == "web" && e.Cfg.PublicURL == "" {
		note += " THICKET_PUBLIC_URL isn't set, so the link points at localhost: it only works for someone who can reach this machine unless they swap in the right address."
	}
	if a.OwnSprig {
		note += " When " + name + " arrives I'll greet them and help them make their own sprig. Tell the user once, plainly: " + name +
			"'s sprig will be private. Thicket will show the user only that it exists, what it costs and whether it's working, never its chats, memories or activity. " +
			"As whoever runs the server, they could technically open the database file, but Thicket won't show it to them."
	}
	return note, nil
}

// redeemJoinCode handles a DM that is just an invite code ("join-...",
// or Telegram's /start from an invite link), from an account Thicket
// doesn't know yet.
func (e *Engine) redeemJoinCode(ctx context.Context, in channel.Incoming) error {
	host, ok := e.Chars.Host()
	if !ok {
		return errors.New("no host character is loaded")
	}
	if _, err := e.DB.UserByExternal(ctx, in.Channel, in.UserID); err == nil {
		return e.send(ctx, in.ChatRef(), host, "You're already set up here, so that invite isn't needed. Just say hi!")
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
	e.linkMu.Unlock()
	m := joinCodeRe.FindStringSubmatch(in.Text)
	e.scrub(ctx, in)
	u, inv, err := e.DB.RedeemInviteChat(ctx, store.HashToken(m[1]), in.Channel, in.UserID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		e.linkMu.Lock()
		if e.linkFails == nil {
			e.linkFails = map[string][]time.Time{}
		}
		e.linkFails[who] = append(recent, now)
		e.linkMu.Unlock()
		return e.send(ctx, in.ChatRef(), host, "That invite has expired or was already used. Ask whoever invited you for a new one.")
	case errors.Is(err, store.ErrIdentityTaken):
		return e.send(ctx, in.ChatRef(), host, "You're already set up here, so that invite isn't needed. Just say hi!")
	case err != nil:
		return err
	}
	e.joined(ctx, u, inv.CreatedBy, in.Channel)
	return e.Welcome(ctx, u, in.ChatRef())
}

// joined records that someone accepted an invite.
func (e *Engine) joined(ctx context.Context, u *store.User, by int64, app string) {
	name := u.Name
	if name == "" {
		name = fmt.Sprintf("user %d", u.ID)
	}
	e.addTrail(ctx, by, "", store.TrailRole, fmt.Sprintf("%s joined from an invite (%s, as %s)", name, app, u.Role), "",
		&trailUndo{Type: "remove_user", UserID: u.ID})
	e.logActivity(ctx, "info", "system", "", u.ID, name+" accepted an invite on "+app, "")
}

// WebJoined is called when someone accepts an invite in the web app: it
// goes in the trail, and Juni greets them in their chat, in the
// background (WaitTasks waits for it).
func (e *Engine) WebJoined(ctx context.Context, u *store.User) {
	e.joined(ctx, u, u.InvitedBy, "web")
	e.tasks.Add(1)
	go func() {
		defer e.tasks.Done()
		if err := e.Welcome(ctx, u, ""); err != nil {
			e.Log.Warn("welcome a new person", "user", u.ID, "err", err)
		}
	}()
}

// Welcome has the right sprig say hello to someone who just joined:
// Juni for adults (starting their short tour), their kid sprig for a
// kid. chatRef is where they wrote from; "" sends to their DMs.
func (e *Engine) Welcome(ctx context.Context, u *store.User, chatRef string) error {
	if !e.ModelReady(ctx) {
		return nil // no model yet: they'll be greeted when they write
	}
	char := e.defaultSprigFor(u)
	if char == nil {
		if chatRef != "" {
			if host, ok := e.Chars.Host(); ok {
				return e.send(ctx, chatRef, host, "Welcome! There's no sprig set up for you yet. A grown-up will set one up for you soon.")
			}
		}
		return nil
	}
	inviter := "the person who invited them"
	if u.InvitedBy != 0 {
		if by, err := e.DB.UserByID(ctx, u.InvitedBy); err == nil && by.Name != "" {
			inviter = by.Name
		}
	}
	event := fmt.Sprintf("[Thicket event] %s just joined this Thicket through an invite from %s, and this is your first message to them. "+
		"Greet them warmly by name, in a few sentences, and start their short tour (see your instructions). Talk to them, not to %s.",
		nameOr(u.Name, "Someone new"), inviter, inviter)
	if u.IsKid() {
		event = "[Thicket event] A kid you're set up for just arrived. Say a short, friendly, kid-sized hello and ask what they'd like to do. Nothing about grown-up features."
	}
	t := &turn{user: u, char: char, query: event, noTools: true, isDM: true}
	text, err := e.runTurn(ctx, t, event)
	if err != nil {
		return err
	}
	if strings.TrimSpace(text) == "" {
		return nil
	}
	if chatRef == "" {
		return e.sendToUser(ctx, u, char, text)
	}
	conv, err := e.DB.OpenConversation(ctx, u.ID, chatRef, e.Cfg.IdleAfter)
	if err != nil {
		return err
	}
	if err := e.send(ctx, chatRef, char, text); err != nil {
		return err
	}
	e.active.Store(chatRef, char.ID)
	return e.DB.AddMessage(ctx, conv.ID, "assistant", char.ID, text)
}

// sandboxState reports whether the sandbox computer is connected, and
// whether it is but no sprig may use it yet. When it's turned off, sprigs
// say nothing about it.
func (e *Engine) sandboxState() (connected, idle bool) {
	if e.MCP == nil {
		return false, false
	}
	if !strings.HasPrefix(e.MCP.Status()["sandbox"], "connected") {
		return false, false
	}
	for _, c := range e.Chars.All() {
		for _, sc := range c.Tools {
			if strings.HasPrefix(sc, character.ScopeMCPPrefix+"sandbox/") {
				return true, false
			}
		}
	}
	return true, true
}

func nameOr(name, fallback string) string {
	if name == "" {
		return fallback
	}
	return name
}

// privateLabel names someone's solo sprig without saying anything about
// it: "Kim's private sprig".
func (e *Engine) privateLabel(ctx context.Context, c *character.Character) string {
	if o, err := e.DB.UserByID(ctx, c.BelongsTo); err == nil && o.Name != "" {
		return o.Name + "'s private sprig"
	}
	return fmt.Sprintf("user %d's private sprig", c.BelongsTo)
}
