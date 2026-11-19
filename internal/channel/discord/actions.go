package discord

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/bwmarrin/discordgo"

	"github.com/theshield2594/thicket/internal/channel"
)

// Buttons are message components. Discord only delivers presses to the
// application that posted them, so a message with buttons goes through
// the bot, or through the persona webhook when that webhook is
// application-owned (created by this bot, as Thicket's are).

var errNotAppHook = errors.New("webhook isn't application-owned")

// SendActions posts text as a persona with buttons under its last part.
func (a *Adapter) SendActions(ctx context.Context, chatID string, p channel.Persona, text string, actions []channel.Action) ([]string, error) {
	if a.s == nil {
		return nil, fmt.Errorf("discord is not connected")
	}
	parts := split(text, 1990)
	if len(parts) == 0 {
		parts = []string{"…"}
	}
	var ids []string
	if len(parts) > 1 {
		var err error
		if ids, err = a.SendIDs(ctx, chatID, p, strings.Join(parts[:len(parts)-1], "\n")); err != nil {
			return ids, err
		}
	}
	last, comps := parts[len(parts)-1], components(actions)
	if ch := a.channel(chatID); ch != nil && ch.Type != discordgo.ChannelTypeDM && ch.Type != discordgo.ChannelTypeGroupDM {
		id, err := a.sendHookActions(ch, p, last, comps)
		if err == nil {
			return append(ids, id), nil
		}
		if !errors.Is(err, errNotAppHook) {
			a.log.Warn("webhook send with buttons failed, falling back to bot message", "channel", chatID, "err", err)
		}
	}
	m, err := a.s.ChannelMessageSendComplex(chatID, &discordgo.MessageSend{
		Embeds:          []*discordgo.MessageEmbed{{Author: &discordgo.MessageEmbedAuthor{Name: p.Name, IconURL: p.AvatarURL}, Description: last, Color: p.Color}},
		Components:      comps,
		AllowedMentions: allowedMentions,
	})
	if err != nil {
		return ids, err
	}
	return append(ids, m.ID), nil
}

// sendHookActions posts one message with buttons through the channel's
// webhook, if it is application-owned.
func (a *Adapter) sendHookActions(ch *discordgo.Channel, p channel.Persona, text string, comps []discordgo.MessageComponent) (string, error) {
	parent, thread := ch.ID, ""
	if ch.IsThread() {
		parent, thread = ch.ParentID, ch.ID
	}
	hook, err := a.webhook(parent)
	if err != nil {
		return "", err
	}
	if hook.ApplicationID == "" {
		return "", errNotAppHook
	}
	params := &discordgo.WebhookParams{Content: text, Username: p.Name, AvatarURL: p.AvatarURL, AllowedMentions: allowedMentions, Components: comps}
	var m *discordgo.Message
	if thread != "" {
		m, err = a.s.WebhookThreadExecute(hook.ID, hook.Token, true, thread, params)
	} else {
		m, err = a.s.WebhookExecute(hook.ID, hook.Token, true, params)
	}
	if err != nil {
		return "", err
	}
	if m == nil {
		return "", nil
	}
	return m.ID, nil
}

// components lays buttons out in rows of five (Discord's limit).
func components(actions []channel.Action) []discordgo.MessageComponent {
	var rows []discordgo.MessageComponent
	var row discordgo.ActionsRow
	for _, act := range actions {
		style := discordgo.SecondaryButton
		switch act.Style {
		case channel.ActionPrimary:
			style = discordgo.PrimaryButton
		case channel.ActionDanger:
			style = discordgo.DangerButton
		}
		row.Components = append(row.Components, discordgo.Button{Label: act.Label, Style: style, CustomID: act.ID})
		if len(row.Components) == 5 {
			rows, row = append(rows, row), discordgo.ActionsRow{}
		}
	}
	if len(row.Components) > 0 {
		rows = append(rows, row)
	}
	return rows
}

// press handles a button press. Discord wants an answer within three
// seconds, so it is acknowledged first; then the notice goes to the
// presser alone (an ephemeral follow-up) and a settled message is edited
// to show the outcome without its buttons.
func (a *Adapter) press(ctx context.Context, i *discordgo.Interaction) {
	if err := a.s.InteractionRespond(i, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseDeferredMessageUpdate}); err != nil {
		a.log.Warn("discord: couldn't acknowledge a button press", "err", err)
		return
	}
	u := i.User
	if i.Member != nil && i.Member.User != nil {
		u = i.Member.User
	}
	var r channel.PressReply
	switch {
	case u == nil || u.Bot:
		return
	case a.onPress == nil:
		r.Notice = "That button doesn't do anything here."
	default:
		name := u.GlobalName
		if name == "" {
			name = u.Username
		}
		msgID := ""
		if i.Message != nil {
			msgID = i.Message.ID
		}
		r = a.onPress(ctx, channel.Press{
			Channel: "discord", ChatID: i.ChannelID, MessageID: msgID, UserID: u.ID, UserName: name,
			IsDM: i.GuildID == "", ActionID: i.MessageComponentData().CustomID,
		})
	}
	if r.Notice != "" {
		if _, err := a.s.FollowupMessageCreate(i, false, &discordgo.WebhookParams{Content: r.Notice, Flags: discordgo.MessageFlagsEphemeral,
			AllowedMentions: &discordgo.MessageAllowedMentions{}}); err != nil {
			a.log.Debug("discord: couldn't send a private notice", "err", err)
		}
	}
	if r.Outcome != "" {
		if _, err := a.s.InteractionResponseEdit(i, settled(i.Message, r.Outcome)); err != nil {
			a.log.Warn("discord: couldn't remove a message's buttons", "err", err)
		}
	}
}

// settled is the edit that removes a message's buttons and shows the
// outcome: in the footer of its last embed, or in an embed of its own.
func settled(m *discordgo.Message, outcome string) *discordgo.WebhookEdit {
	none := []discordgo.MessageComponent{}
	var embeds []*discordgo.MessageEmbed
	if m != nil {
		for _, e := range m.Embeds {
			c := *e
			embeds = append(embeds, &c)
		}
	}
	if n := len(embeds); n > 0 {
		embeds[n-1].Footer = &discordgo.MessageEmbedFooter{Text: outcome}
	} else {
		embeds = []*discordgo.MessageEmbed{{Description: outcome}}
	}
	return &discordgo.WebhookEdit{Components: &none, Embeds: &embeds}
}

// SettleActions settles a message sent with buttons that nobody pressed:
// its buttons are removed and the outcome shown, as after a press. A
// message the persona webhook posted is edited through that webhook;
// anything else through the bot.
func (a *Adapter) SettleActions(ctx context.Context, chatID, messageID, outcome string) error {
	if a.s == nil {
		return fmt.Errorf("discord is not connected")
	}
	m, err := a.s.ChannelMessage(chatID, messageID, discordgo.WithContext(ctx))
	if err != nil {
		return err
	}
	edit := settled(m, outcome)
	if m.WebhookID == "" {
		_, err = a.s.ChannelMessageEditComplex(&discordgo.MessageEdit{ID: messageID, Channel: chatID, Components: edit.Components, Embeds: edit.Embeds},
			discordgo.WithContext(ctx))
		return err
	}
	parent, thread := chatID, ""
	if ch := a.channel(chatID); ch != nil && ch.IsThread() {
		parent, thread = ch.ParentID, ch.ID
	}
	hook, err := a.webhook(parent)
	if err != nil {
		return err
	}
	if hook.ID != m.WebhookID {
		return errors.New("the message was posted by another webhook")
	}
	uri := discordgo.EndpointWebhookMessage(hook.ID, hook.Token, messageID)
	if thread != "" {
		uri += "?thread_id=" + thread
	}
	_, err = a.s.RequestWithBucketID("PATCH", uri, edit, discordgo.EndpointWebhookToken("", ""), discordgo.WithContext(ctx))
	return err
}
