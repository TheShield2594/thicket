// Package discord is the Discord channel adapter. In server channels each
// character posts through a webhook with its own name and avatar; in DMs
// (where webhooks don't exist) replies carry the character in an embed.
package discord

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"

	"github.com/theshield2594/thicket/internal/channel"
)

const webhookName = "Thicket"

type Adapter struct {
	token  string
	listen map[string]bool
	log    *slog.Logger

	s         *discordgo.Session
	connected atomic.Bool

	mu       sync.Mutex
	hooks    map[string]*discordgo.Webhook // parent channel id -> webhook
	noHooks  map[string]bool               // channels where we lack permission
	chanInfo map[string]*discordgo.Channel
}

func New(token string, listenChannels []string, log *slog.Logger) *Adapter {
	l := map[string]bool{}
	for _, c := range listenChannels {
		l[c] = true
	}
	return &Adapter{token: token, listen: l, log: log, hooks: map[string]*discordgo.Webhook{},
		noHooks: map[string]bool{}, chanInfo: map[string]*discordgo.Channel{}}
}

func (a *Adapter) Name() string    { return "discord" }
func (a *Adapter) Connected() bool { return a.connected.Load() }

func (a *Adapter) Run(ctx context.Context, h channel.Handler) error {
	s, err := discordgo.New("Bot " + a.token)
	if err != nil {
		return err
	}
	s.Identify.Intents = discordgo.IntentsGuildMessages | discordgo.IntentsDirectMessages | discordgo.IntentsMessageContent
	s.AddHandler(func(_ *discordgo.Session, _ *discordgo.Connect) { a.connected.Store(true) })
	s.AddHandler(func(_ *discordgo.Session, _ *discordgo.Disconnect) { a.connected.Store(false) })
	s.AddHandler(func(_ *discordgo.Session, r *discordgo.Ready) {
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
		for _, att := range m.Attachments {
			text += fmt.Sprintf("\n[attachment: %s]", att.Filename)
		}
		name := m.Author.GlobalName
		if name == "" {
			name = m.Author.Username
		}
		in := channel.Incoming{
			Channel: "discord", ChatID: m.ChannelID, MessageID: m.ID,
			UserID: m.Author.ID, UserName: name, Text: strings.TrimSpace(text),
			IsDM: m.GuildID == "", Mentioned: mentioned, Listening: a.listen[m.ChannelID],
		}
		go h(ctx, in)
	})
	if err := s.Open(); err != nil {
		return fmt.Errorf("%w (check DISCORD_TOKEN and that the Message Content intent is enabled)", err)
	}
	a.s = s
	<-ctx.Done()
	a.connected.Store(false)
	return s.Close()
}

var allowedMentions = &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{discordgo.AllowedMentionTypeUsers}}

func (a *Adapter) Send(ctx context.Context, chatID string, p channel.Persona, text string) error {
	if a.s == nil {
		return fmt.Errorf("discord is not connected")
	}
	ch := a.channel(chatID)
	if ch != nil && ch.Type != discordgo.ChannelTypeDM && ch.Type != discordgo.ChannelTypeGroupDM {
		if err := a.sendWebhook(ch, p, text); err == nil {
			return nil
		} else {
			a.log.Warn("webhook send failed, falling back to bot message", "channel", chatID, "err", err)
		}
		for _, part := range split(text, 1900) {
			if _, err := a.s.ChannelMessageSendComplex(chatID, &discordgo.MessageSend{
				Content: "**" + p.Name + ":** " + part, AllowedMentions: allowedMentions,
			}); err != nil {
				return err
			}
		}
		return nil
	}
	for _, part := range split(text, 4000) {
		emb := &discordgo.MessageEmbed{
			Author:      &discordgo.MessageEmbedAuthor{Name: p.Name, IconURL: p.AvatarURL},
			Description: part,
			Color:       p.Color,
		}
		if _, err := a.s.ChannelMessageSendComplex(chatID, &discordgo.MessageSend{Embeds: []*discordgo.MessageEmbed{emb}, AllowedMentions: allowedMentions}); err != nil {
			return err
		}
	}
	return nil
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

func (a *Adapter) sendWebhook(ch *discordgo.Channel, p channel.Persona, text string) error {
	parent, thread := ch.ID, ""
	if ch.IsThread() {
		parent, thread = ch.ParentID, ch.ID
	}
	hook, err := a.webhook(parent)
	if err != nil {
		return err
	}
	for _, part := range split(text, 1990) {
		params := &discordgo.WebhookParams{Content: part, Username: p.Name, AvatarURL: p.AvatarURL, AllowedMentions: allowedMentions}
		if thread != "" {
			_, err = a.s.WebhookThreadExecute(hook.ID, hook.Token, false, thread, params)
		} else {
			_, err = a.s.WebhookExecute(hook.ID, hook.Token, false, params)
		}
		if err != nil {
			a.mu.Lock()
			delete(a.hooks, parent) // maybe deleted by a user; recreate next time
			a.mu.Unlock()
			return err
		}
	}
	return nil
}

func (a *Adapter) webhook(channelID string) (*discordgo.Webhook, error) {
	a.mu.Lock()
	if h := a.hooks[channelID]; h != nil {
		a.mu.Unlock()
		return h, nil
	}
	if a.noHooks[channelID] {
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
		a.mu.Lock()
		a.noHooks[channelID] = true
		a.mu.Unlock()
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
