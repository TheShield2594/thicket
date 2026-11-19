// Package discord is the Discord channel adapter. In server channels each
// character posts through a webhook with its own name and avatar; in DMs
// (where webhooks don't exist) replies carry the character in an embed.
package discord

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
	"github.com/gorilla/websocket"

	"github.com/theshield2594/thicket/internal/channel"
)

const webhookName = "Thicket"

type Adapter struct {
	token  string
	listen map[string]bool
	log    *slog.Logger

	s         *discordgo.Session
	connected atomic.Bool
	me        atomic.Pointer[discordgo.User]

	mu       sync.Mutex
	hooks    map[string]*discordgo.Webhook // parent channel id -> webhook
	noHooks  map[string]time.Time          // channels where we lack permission, until when
	chanInfo map[string]*discordgo.Channel
	onDelete func(ctx context.Context, d channel.Deletion)
	onReact  func(ctx context.Context, r channel.Reaction)
}

// OnReaction registers what to call when someone adds a reaction. Call it
// before Run.
func (a *Adapter) OnReaction(f func(ctx context.Context, r channel.Reaction)) { a.onReact = f }

// OnDelete registers what to call when someone deletes messages. Call it
// before Run.
func (a *Adapter) OnDelete(f func(ctx context.Context, d channel.Deletion)) { a.onDelete = f }

// ownHook reports whether a webhook id is one of ours (a sprig's post).
func (a *Adapter) ownHook(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, h := range a.hooks {
		if h.ID == id {
			return true
		}
	}
	return false
}

func New(token string, listenChannels []string, log *slog.Logger) *Adapter {
	l := map[string]bool{}
	for _, c := range listenChannels {
		l[c] = true
	}
	return &Adapter{token: token, listen: l, log: log, hooks: map[string]*discordgo.Webhook{},
		noHooks: map[string]time.Time{}, chanInfo: map[string]*discordgo.Channel{}}
}

func (a *Adapter) Name() string    { return "discord" }
func (a *Adapter) Connected() bool { return a.connected.Load() }

// invitePermissions is what the bot asks for when it's added to a server:
// view channels, send messages (also in threads), manage messages, embed
// links, attach files, read message history and manage webhooks.
const invitePermissions = 1<<10 | 1<<11 | 1<<13 | 1<<14 | 1<<15 | 1<<16 | 1<<29 | 1<<38

// Bot is the bot account and the link that adds it to a server.
func (a *Adapter) Bot() (channel.Bot, bool) {
	u := a.me.Load()
	if u == nil {
		return channel.Bot{}, false
	}
	return channel.Bot{Name: u.Username,
		URL: fmt.Sprintf("https://discord.com/oauth2/authorize?client_id=%s&scope=bot&permissions=%d", u.ID, int64(invitePermissions))}, true
}

// Check asks Discord whether the token is a bot's.
func (a *Adapter) Check(ctx context.Context) error {
	s, err := discordgo.New("Bot " + a.token)
	if err != nil {
		return err
	}
	if _, err := s.User("@me", discordgo.WithContext(ctx)); err != nil {
		var re *discordgo.RESTError
		if errors.As(err, &re) && re.Response != nil && re.Response.StatusCode == http.StatusUnauthorized {
			return errors.New("Discord didn't accept that bot token (copy it again from the Bot page of your application: Reset Token)")
		}
		return fmt.Errorf("couldn't reach Discord: %w", err)
	}
	return nil
}

// openErr explains why the gateway refused the bot.
func openErr(err error) error {
	switch {
	case websocket.IsCloseError(err, 4014):
		return errors.New("Discord refused the connection because the Message Content intent is off: on https://discord.com/developers/applications open your application, Bot, Privileged Gateway Intents, turn on Message Content Intent, then try again")
	case websocket.IsCloseError(err, 4004):
		return errors.New("Discord didn't accept that bot token (copy it again from the Bot page of your application: Reset Token)")
	}
	return fmt.Errorf("%w (check the bot token and that the Message Content intent is enabled)", err)
}

func (a *Adapter) Run(ctx context.Context, h channel.Handler) error {
	s, err := discordgo.New("Bot " + a.token)
	if err != nil {
		return err
	}
	// Reactions are how "remember this" is one tap (🧠 or 📌). They aren't
	// privileged intents, so no setting in the developer portal is needed.
	s.Identify.Intents = discordgo.IntentsGuildMessages | discordgo.IntentsDirectMessages | discordgo.IntentsMessageContent |
		discordgo.IntentsGuildMessageReactions | discordgo.IntentsDirectMessageReactions
	s.AddHandler(func(_ *discordgo.Session, _ *discordgo.Connect) { a.connected.Store(true) })
	s.AddHandler(func(_ *discordgo.Session, _ *discordgo.Disconnect) { a.connected.Store(false) })
	s.AddHandler(func(_ *discordgo.Session, r *discordgo.Ready) {
		a.me.Store(r.User)
		a.connected.Store(true)
		a.log.Info("discord connected", "as", r.User.Username, "guilds", len(r.Guilds))
	})
	s.AddHandler(func(s *discordgo.Session, m *discordgo.MessageCreate) {
		// Never react to bots or webhooks (including our own characters):
		// this is the first line of defence against bot-to-bot loops.
		if m.Author == nil || m.Author.Bot || m.WebhookID != "" {
			return
		}
		text := m.Content
		mentioned := false
		if s.State != nil && s.State.User != nil {
			for _, u := range m.Mentions {
				if u.ID == s.State.User.ID {
					mentioned = true
				}
			}
			text = strings.ReplaceAll(text, "<@"+s.State.User.ID+">", "")
			text = strings.ReplaceAll(text, "<@!"+s.State.User.ID+">", "")
		}
		text = strings.TrimSpace(text)
		if text == "" && len(m.Attachments) == 0 {
			return
		}
		var atts []channel.Attachment
		for _, att := range m.Attachments {
			atts = append(atts, channel.Attachment{Name: att.Filename, URL: att.URL, ContentType: att.ContentType, Size: att.Size})
		}
		replyTo := ""
		if m.MessageReference != nil && m.MessageReference.MessageID != "" &&
			(m.MessageReference.Type == discordgo.MessageReferenceTypeDefault) {
			replyTo = m.MessageReference.MessageID
		}
		// Replying to a sprig counts as speaking to it, as on Telegram.
		if r := m.ReferencedMessage; r != nil && r.Author != nil && s.State != nil && s.State.User != nil &&
			(r.Author.ID == s.State.User.ID || (r.WebhookID != "" && a.ownHook(r.WebhookID))) {
			mentioned = true
		}
		name := m.Author.GlobalName
		if name == "" {
			name = m.Author.Username
		}
		in := channel.Incoming{
			Channel: "discord", ChatID: m.ChannelID, MessageID: m.ID,
			UserID: m.Author.ID, UserName: name, Text: strings.TrimSpace(text),
			IsDM: m.GuildID == "", Mentioned: mentioned, Listening: a.listen[m.ChannelID],
			Attachments: atts, ReplyTo: replyTo,
		}
		go h(ctx, in)
	})
	s.AddHandler(func(_ *discordgo.Session, m *discordgo.MessageDelete) {
		if a.onDelete != nil && m.Message != nil {
			go a.onDelete(ctx, channel.Deletion{Channel: "discord", ChatID: m.ChannelID, MessageIDs: []string{m.ID}})
		}
	})
	s.AddHandler(func(s *discordgo.Session, r *discordgo.MessageReactionAdd) {
		if a.onReact == nil || r.MessageReaction == nil || r.Emoji.Name == "" {
			return
		}
		if r.Member != nil && r.Member.User != nil && r.Member.User.Bot {
			return
		}
		if s.State != nil && s.State.User != nil && r.UserID == s.State.User.ID {
			return
		}
		go a.onReact(ctx, channel.Reaction{Channel: "discord", ChatID: r.ChannelID, MessageID: r.MessageID, UserID: r.UserID,
			Emoji: r.Emoji.Name, IsDM: r.GuildID == ""})
	})
	s.AddHandler(func(_ *discordgo.Session, m *discordgo.MessageDeleteBulk) {
		if a.onDelete != nil && len(m.Messages) > 0 {
			go a.onDelete(ctx, channel.Deletion{Channel: "discord", ChatID: m.ChannelID, MessageIDs: m.Messages})
		}
	})
	if err := s.Open(); err != nil {
		a.connected.Store(false)
		return openErr(err)
	}
	a.s = s
	<-ctx.Done()
	a.connected.Store(false)
	return s.Close()
}

var allowedMentions = &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{discordgo.AllowedMentionTypeUsers}}

func (a *Adapter) Send(ctx context.Context, chatID string, p channel.Persona, text string) error {
	_, err := a.SendIDs(ctx, chatID, p, text)
	return err
}

// SendIDs posts text as a persona and returns the ids of the messages it
// became.
func (a *Adapter) SendIDs(ctx context.Context, chatID string, p channel.Persona, text string) ([]string, error) {
	if a.s == nil {
		return nil, fmt.Errorf("discord is not connected")
	}
	ch := a.channel(chatID)
	if ch != nil && ch.Type != discordgo.ChannelTypeDM && ch.Type != discordgo.ChannelTypeGroupDM {
		// Only the part the webhook didn't deliver falls back to a bot
		// message, so nothing is posted twice.
		ids, rest, err := a.sendWebhook(ch, p, text)
		if err == nil {
			return ids, nil
		}
		a.log.Warn("webhook send failed, falling back to bot message", "channel", chatID, "err", err)
		for _, part := range split(rest, 1900) {
			m, err := a.s.ChannelMessageSendComplex(chatID, &discordgo.MessageSend{
				Content: "**" + p.Name + ":** " + part, AllowedMentions: allowedMentions,
			})
			if err != nil {
				return ids, err
			}
			ids = append(ids, m.ID)
		}
		return ids, nil
	}
	var ids []string
	for _, part := range split(text, 4000) {
		emb := &discordgo.MessageEmbed{
			Author:      &discordgo.MessageEmbedAuthor{Name: p.Name, IconURL: p.AvatarURL},
			Description: part,
			Color:       p.Color,
		}
		m, err := a.s.ChannelMessageSendComplex(chatID, &discordgo.MessageSend{Embeds: []*discordgo.MessageEmbed{emb}, AllowedMentions: allowedMentions})
		if err != nil {
			return ids, err
		}
		ids = append(ids, m.ID)
	}
	return ids, nil
}

func (a *Adapter) channel(id string) *discordgo.Channel {
	a.mu.Lock()
	c := a.chanInfo[id]
	a.mu.Unlock()
	if c != nil {
		return c
	}
	if a.s.State != nil {
		if c, err := a.s.State.Channel(id); err == nil {
			return c
		}
	}
	c, err := a.s.Channel(id)
	if err != nil {
		return nil
	}
	a.mu.Lock()
	a.chanInfo[id] = c
	a.mu.Unlock()
	return c
}

// sendWebhook posts text in chunks through the channel's webhook,
// returning the posted message ids. On failure it also returns the text
// not yet delivered.
func (a *Adapter) sendWebhook(ch *discordgo.Channel, p channel.Persona, text string) ([]string, string, error) {
	parent, thread := ch.ID, ""
	if ch.IsThread() {
		parent, thread = ch.ParentID, ch.ID
	}
	hook, err := a.webhook(parent)
	if err != nil {
		return nil, text, err
	}
	parts := split(text, 1990)
	var ids []string
	for i, part := range parts {
		params := &discordgo.WebhookParams{Content: part, Username: p.Name, AvatarURL: p.AvatarURL, AllowedMentions: allowedMentions}
		var m *discordgo.Message
		// wait=true: Discord returns the message, so it can be deleted later.
		if thread != "" {
			m, err = a.s.WebhookThreadExecute(hook.ID, hook.Token, true, thread, params)
		} else {
			m, err = a.s.WebhookExecute(hook.ID, hook.Token, true, params)
		}
		if err != nil {
			a.mu.Lock()
			delete(a.hooks, parent) // maybe deleted by a user; recreate next time
			a.mu.Unlock()
			return ids, strings.Join(parts[i:], "\n"), err
		}
		if m != nil {
			ids = append(ids, m.ID)
		}
	}
	return ids, "", nil
}

// noHookRetry is how long a missing-permission result is trusted before
// trying to create a webhook again (permissions can be granted later).
const noHookRetry = 10 * time.Minute

func isForbidden(err error) bool {
	var re *discordgo.RESTError
	return errors.As(err, &re) && re.Response != nil && re.Response.StatusCode == http.StatusForbidden
}

func (a *Adapter) webhook(channelID string) (*discordgo.Webhook, error) {
	a.mu.Lock()
	if h := a.hooks[channelID]; h != nil {
		a.mu.Unlock()
		return h, nil
	}
	if until, ok := a.noHooks[channelID]; ok && time.Now().Before(until) {
		a.mu.Unlock()
		return nil, fmt.Errorf("no Manage Webhooks permission")
	}
	a.mu.Unlock()
	hooks, err := a.s.ChannelWebhooks(channelID)
	if err == nil {
		for _, h := range hooks {
			if h.Name == webhookName && h.Token != "" && a.s.State != nil && h.User != nil && h.User.ID == a.s.State.User.ID {
				a.mu.Lock()
				a.hooks[channelID] = h
				a.mu.Unlock()
				return h, nil
			}
		}
	}
	h, err := a.s.WebhookCreate(channelID, webhookName, "")
	if err != nil {
		if isForbidden(err) { // cache only a real permission refusal, not a blip
			a.mu.Lock()
			a.noHooks[channelID] = time.Now().Add(noHookRetry)
			a.mu.Unlock()
		}
		return nil, fmt.Errorf("create webhook (give the bot Manage Webhooks so each sprig gets its own name and avatar): %w", err)
	}
	a.mu.Lock()
	a.hooks[channelID] = h
	a.mu.Unlock()
	return h, nil
}

func (a *Adapter) SendDirect(ctx context.Context, userID string, p channel.Persona, text string) (string, error) {
	if a.s == nil {
		return "", fmt.Errorf("discord is not connected")
	}
	ch, err := a.s.UserChannelCreate(userID)
	if err != nil {
		return "", err
	}
	return ch.ID, a.Send(ctx, ch.ID, p, text)
}

func (a *Adapter) Delete(ctx context.Context, chatID, messageID string) error {
	if a.s == nil {
		return fmt.Errorf("discord is not connected")
	}
	// A sprig's post in a server channel came through our webhook, which
	// can delete it without the Manage Messages permission.
	if ch := a.channel(chatID); ch != nil && !ch.IsThread() && ch.Type != discordgo.ChannelTypeDM && ch.Type != discordgo.ChannelTypeGroupDM {
		a.mu.Lock()
		hook := a.hooks[ch.ID]
		a.mu.Unlock()
		if hook != nil && a.s.WebhookMessageDelete(hook.ID, hook.Token, messageID) == nil {
			return nil
		}
	}
	return a.s.ChannelMessageDelete(chatID, messageID)
}

func (a *Adapter) Typing(ctx context.Context, chatID string) {
	if a.s != nil {
		_ = a.s.ChannelTyping(chatID)
	}
}

// split breaks text into chunks no longer than n, preferring line breaks.
func split(text string, n int) []string {
	var out []string
	for len(text) > n {
		cut := strings.LastIndex(text[:n], "\n")
		if cut < n/2 {
			cut = strings.LastIndex(text[:n], " ")
		}
		if cut < n/2 {
			cut = n
			for cut > 0 && !utf8.RuneStart(text[cut]) {
				cut--
			}
		}
		out = append(out, strings.TrimSpace(text[:cut]))
		text = strings.TrimSpace(text[cut:])
	}
	if text != "" {
		out = append(out, text)
	}
	return out
}
