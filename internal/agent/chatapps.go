package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/theshield2594/thicket/internal/channel"
	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/secrets"
	"github.com/theshield2594/thicket/internal/store"
)

// chatApp is a chat app that connects with a bot token.
type chatApp struct {
	id, label string
	secret    string // the name its token is stored under
	howTo     string // where the token comes from
}

var chatApps = []chatApp{
	{"discord", "Discord", secrets.DiscordToken,
		"On https://discord.com/developers/applications: New Application, then Bot. Turn on **Message Content Intent** (under Privileged Gateway Intents), press **Reset Token** and copy it."},
	{"telegram", "Telegram", secrets.TelegramToken,
		"In Telegram, message @BotFather, send /newbot, and copy the token it gives you."},
	{"matrix", "Matrix", secrets.MatrixToken,
		"Make a Matrix account for the bot on your homeserver, sign in to it once (in Element: Settings, Help & About, Access Token) and copy its access token. THICKET_MATRIX_HOMESERVER must name the homeserver (see docs/matrix.md)."},
}

func chatAppByID(id string) *chatApp {
	for i := range chatApps {
		if chatApps[i].id == strings.ToLower(strings.TrimSpace(id)) {
			return &chatApps[i]
		}
	}
	return nil
}

func chatAppBySecret(name string) *chatApp {
	for i := range chatApps {
		if chatApps[i].secret == name {
			return &chatApps[i]
		}
	}
	return nil
}

var (
	discordTokenRe  = regexp.MustCompile(`[MNO][A-Za-z0-9_\-]{22,27}\.[A-Za-z0-9_\-]{6,7}\.[A-Za-z0-9_\-]{27,}`)
	telegramTokenRe = regexp.MustCompile(`\b[0-9]{6,12}:AA[A-Za-z0-9_\-]{33}`)
	// A Synapse access token; other homeservers' tokens have no fixed shape.
	matrixTokenRe = regexp.MustCompile(`\bsyt_[A-Za-z0-9_\-]{20,}`)
)

// chatToken finds a Discord, Telegram or Matrix bot token in text.
func chatToken(text string) (*chatApp, string) {
	if tok := telegramTokenRe.FindString(text); tok != "" {
		return chatAppByID("telegram"), tok
	}
	if tok := matrixTokenRe.FindString(text); tok != "" {
		return chatAppByID("matrix"), tok
	}
	for _, tok := range discordTokenRe.FindAllString(text, -1) {
		// A Discord token starts with the bot's numeric id in base64.
		id, err := base64.RawStdEncoding.DecodeString(tok[:strings.IndexByte(tok, '.')])
		if err != nil || len(id) < 15 || strings.Trim(string(id), "0123456789") != "" {
			continue
		}
		return chatAppByID("discord"), tok
	}
	return nil, ""
}

// chatBot is the bot account a connected chat app runs as.
func (e *Engine) chatBot(app string) (channel.Bot, bool) {
	ch := e.channelFor(app)
	if ch == nil || !ch.Connected() {
		return channel.Bot{}, false
	}
	if id, ok := ch.(channel.Identity); ok {
		if b, ok := id.Bot(); ok {
			return b, true
		}
	}
	return channel.Bot{}, true
}

// chatNextSteps is what to do once a chat app is connected. from is the
// app the user is talking on now.
func (e *Engine) chatNextSteps(app *chatApp, from string) string {
	bot, _ := e.chatBot(app.id)
	var b strings.Builder
	switch app.id {
	case "discord":
		if bot.URL != "" {
			b.WriteString("\n- Add the bot to your server: " + bot.URL)
		}
		if from != app.id {
			b.WriteString("\n- To talk to me on Discord as yourself, say **link my Discord** and I'll give you a one-time code to DM the bot.")
		}
		b.WriteString("\n- To let someone else in, give me their Discord user ID.")
	case "telegram":
		if bot.URL != "" {
			b.WriteString("\n- Open a chat with the bot: " + bot.URL)
		}
		if from != app.id {
			b.WriteString("\n- To talk to me on Telegram as yourself, say **link my Telegram** and I'll give you a one-time code to send the bot.")
		}
		b.WriteString("\n- To let someone else in, give me their numeric Telegram ID (@userinfobot shows it).")
	case "matrix":
		if bot.Name != "" {
			b.WriteString("\n- Start a chat with " + bot.Name + " (encryption off: encrypted rooms aren't supported yet), or invite it to a room.")
		}
		if from != app.id {
			b.WriteString("\n- To talk to me on Matrix as yourself, say **link my Matrix** and I'll give you a one-time code to send the bot.")
		}
		b.WriteString("\n- To let someone else in, give me their Matrix ID, like @alice:example.org.")
	}
	return b.String()
}

func botName(b channel.Bot) string {
	if b.Name == "" {
		return ""
	}
	return " as **" + b.Name + "**"
}

// connectChat brings a chat app online with token and, once it works,
// stores the token so the app comes back after a restart.
func (e *Engine) connectChat(ctx context.Context, app *chatApp, token string) error {
	if e.ConnectChat == nil {
		return fmt.Errorf("this Thicket can't connect %s while it runs: store the token with `thicket secrets set %s` and restart", app.label, app.secret)
	}
	e.Secrets.Remember(token) // never logged or shown, even if it fails
	cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := e.ConnectChat(cctx, app.id, token); err != nil {
		return err
	}
	if err := e.Secrets.Put(ctx, app.secret, token); err != nil {
		return fmt.Errorf("%s is connected, but the token couldn't be stored (%v), so it won't come back after a restart", app.label, err)
	}
	return nil
}

// connectChatToken handles a bot token an admin sent in a DM: Thicket
// connects the chat app itself, with no model involved, and says how it
// went.
func (e *Engine) connectChatToken(ctx context.Context, in channel.Incoming, user *store.User, host *character.Character, app *chatApp, token string) error {
	e.scrub(ctx, in)
	_ = e.DB.ClearPendingSecret(ctx, user.ID)
	if ch := e.channelFor(in.Channel); ch != nil {
		ch.Typing(ctx, in.ChatID)
	}
	if err := e.connectChat(ctx, app, token); err != nil {
		e.logActivity(ctx, "warn", "system", "", user.ID, "connecting "+app.label+" failed: "+shortErr(err), "")
		e.noteEvent(ctx, in, user, fmt.Sprintf("The user sent a %s bot token. Thicket tried to connect %s with it and failed: %s. Thicket already told them; the token wasn't kept.", app.label, app.label, err))
		return e.send(ctx, in.ChatRef(), host, fmt.Sprintf("I tried to connect %s with that token, but it didn't work: %s\n\nI haven't kept the token. Send it again once that's sorted and I'll retry.", app.label, err))
	}
	bot, _ := e.chatBot(app.id)
	steps := e.chatNextSteps(app, in.Channel)
	e.logActivity(ctx, "info", "system", "", user.ID, app.label+" connected", "")
	e.noteEvent(ctx, in, user, fmt.Sprintf("The user sent a %s bot token. Thicket connected %s%s and stored the token encrypted; no restart or environment change is needed. Thicket already told them so, with these next steps:%s", app.label, app.label, botName(bot), steps))
	return e.send(ctx, in.ChatRef(), host, fmt.Sprintf("%s is connected%s. I've stored the token encrypted, so it reconnects by itself after a restart: nothing to add to your env file.\n\nNext:%s", app.label, botName(bot), steps))
}

// noteEvent records something Thicket did in the conversation, so the
// sprig knows about it on its next reply.
func (e *Engine) noteEvent(ctx context.Context, in channel.Incoming, user *store.User, event string) {
	conv, err := e.DB.OpenConversation(ctx, user.ID, in.ChatRef(), e.Cfg.IdleAfter)
	if err != nil {
		e.Log.Warn("note event", "err", err)
		return
	}
	_ = e.DB.AddMessage(ctx, conv.ID, "event", "", event)
}

func (e *Engine) toolConnectChatApp(ctx context.Context, t *turn, args json.RawMessage) (string, error) {
	a, err := decode[struct {
		App    string `json:"app"`
		Secret string `json:"secret"`
	}](args)
	if err != nil {
		return "", err
	}
	app := chatAppByID(a.App)
	if app == nil {
		return "", errors.New("app must be discord, telegram or matrix")
	}
	from, _ := splitRef(t.chatRef)
	// A token the user already sent, under another name or from an earlier try.
	name := app.secret
	if a.Secret != "" {
		if name = a.Secret; !secrets.ValidName(name) {
			return "", errors.New("secret is the name of a stored secret, never a token itself")
		}
	}
	if bot, ok := e.chatBot(app.id); ok && a.Secret == "" {
		return fmt.Sprintf("%s is already connected%s. Pass these on:%s\nTo switch to another bot, the user sends its token here in a DM.", app.label, botName(bot), e.chatNextSteps(app, from)), nil
	}
	token, gerr := e.Secrets.Get(ctx, name)
	if gerr != nil && a.Secret != "" {
		return "", fmt.Errorf("no stored secret is called %q", name)
	}
	if gerr == nil {
		cerr := e.connectChat(ctx, app, token)
		if cerr == nil {
			if name != app.secret {
				_ = e.Secrets.Delete(ctx, name) // it now lives under the app's own name
			}
			bot, _ := e.chatBot(app.id)
			e.logActivity(ctx, "info", "system", t.char.ID, t.user.ID, app.label+" connected", "")
			return fmt.Sprintf("%s is now connected%s, with the token stored encrypted. No restart or environment change is needed. Pass these on:%s", app.label, botName(bot), e.chatNextSteps(app, from)), nil
		}
		if a.Secret != "" || e.ConnectChat == nil {
			return "", cerr
		}
		err = cerr // the stored token no longer works: ask for a new one
	}
	if !t.isDM {
		return "", errors.New("bot tokens are only collected in a DM; ask the user to DM you")
	}
	if e.ConnectChat == nil {
		return "", fmt.Errorf("this Thicket can't connect %s while it runs: the token is stored with `thicket secrets set %s`, then Thicket is restarted", app.label, app.secret)
	}
	if perr := e.DB.SetPendingSecret(ctx, store.PendingSecret{UserID: t.user.ID, Name: app.secret, Purpose: app.label + " bot token"}); perr != nil {
		return "", perr
	}
	out := fmt.Sprintf("Ready. Tell the user how to get a bot token: %s Then they send the token as their next message here (or 'cancel'). Thicket connects %s straight away and answers with what to do next; no restart or environment change is needed, and you never see the token.", app.howTo, app.label)
	if err != nil {
		out = fmt.Sprintf("The stored %s token didn't work (%s). ", app.label, shortErr(err)) + out
	}
	return out, nil
}
