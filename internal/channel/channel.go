// Package channel defines the interface every chat adapter implements.
// Discord, Telegram and the web app implement it (plus the internal
// "api" channel of the local HTTP API).
package channel

import (
	"bytes"
	"context"
	"strings"
)

// Persona is how a character appears in a chat.
type Persona struct {
	// ID is the character id. Adapters that keep one thread per character
	// (the web app) and the local API use it; Discord ignores it.
	ID        string
	Name      string
	AvatarURL string
	Color     int
}

// Incoming is a user message, normalised across channels.
type Incoming struct {
	Channel   string // adapter name, e.g. "discord"
	ChatID    string // where to reply
	MessageID string
	UserID    string // channel-specific user id
	UserName  string
	Text      string
	IsDM      bool
	// Mentioned is true when the bot itself was @-mentioned.
	Mentioned bool
	// Listening is true for group chats where the bot answers everything.
	Listening bool
	// UserRef is the Thicket user id when the adapter has already
	// authenticated the sender (the web app, the local API). 0 means
	// "look up UserID".
	UserRef int64
	// Character pins the chat to one sprig (a per-sprig web thread). It
	// still yields to a sprig addressed by name.
	Character string
	// Attachments are files sent with the message. Text holds only what
	// the user typed; the engine decides what to download.
	Attachments []Attachment
	// NoApprovals: this message comes from an agent acting for the user
	// (the MCP server), so it can't answer an approval prompt. The person
	// answers on the dashboard or in a chat app instead.
	NoApprovals bool
	// ReplyTo is the platform id of the message this one replies to, if
	// any ("forget this", "pin this" in reply to a message).
	ReplyTo string
}

// Attachment is a file sent with a message. URL must be https and is
// fetched with size limits (see Fetch).
type Attachment struct {
	Name        string
	URL         string
	ContentType string // as reported by the platform; may be empty
	Size        int    // bytes, as reported by the platform; 0 if unknown
	// Open downloads the file when a plain URL won't do (the platform
	// needs an authenticated call). Its errors must not contain secrets.
	Open func(ctx context.Context, max int) ([]byte, string, error)
}

// ChatRef is a channel-qualified chat id, stored in the database.
func (in Incoming) ChatRef() string { return in.Channel + ":" + in.ChatID }

type Handler func(ctx context.Context, in Incoming)

type Channel interface {
	Name() string
	// Run connects and delivers messages to h until ctx ends.
	Run(ctx context.Context, h Handler) error
	// Send posts text to a chat as a persona.
	Send(ctx context.Context, chatID string, p Persona, text string) error
	// SendDirect opens (or reuses) a private chat with a user and posts there.
	SendDirect(ctx context.Context, userID string, p Persona, text string) (chatID string, err error)
	// Delete removes a message (used to scrub pasted secrets).
	Delete(ctx context.Context, chatID, messageID string) error
	// Typing shows a typing indicator, best effort.
	Typing(ctx context.Context, chatID string)
	// Connected reports whether the adapter is currently online.
	Connected() bool
}

// Poster is implemented by channels that can say which messages a send
// posted (a long text may become several), so they can be deleted later.
type Poster interface {
	SendIDs(ctx context.Context, chatID string, p Persona, text string) ([]string, error)
}

// Voice is a spoken reply: audio of a sprig reading (all or a summary of)
// a reply that was also sent as text.
type Voice struct {
	Audio []byte
	// Format is the audio's format: "ogg" (Ogg Opus), "mp3", "wav",
	// "aac" or "flac". See VoiceFormat.
	Format string
	// Text is what is said.
	Text string
	// With is the platform id of the text message this goes with, when
	// the channel said (Poster); adapters may attach the audio to it.
	With string
}

// VoiceSender is implemented by channels that can post a spoken reply:
// Telegram a voice note, Discord an audio file, the web app a player.
type VoiceSender interface {
	// SpeechFormat is the speech server response_format the channel
	// wants: "opus" for Telegram voice notes, "mp3" elsewhere.
	SpeechFormat() string
	// SendVoice returns the ids of any messages the audio became (none
	// when it was attached to the text message), so deleting the reply
	// deletes the audio too.
	SendVoice(ctx context.Context, chatID string, p Persona, v Voice) ([]string, error)
}

// VoiceFormat names the format of audio from its first bytes, falling
// back to its content type: "ogg", "mp3", "wav", "flac", "aac", or "" if
// it can't tell.
func VoiceFormat(b []byte, contentType string) string {
	switch {
	case bytes.HasPrefix(b, []byte("OggS")):
		return "ogg"
	case bytes.HasPrefix(b, []byte("ID3")), len(b) > 1 && b[0] == 0xFF && b[1]&0xE0 == 0xE0 && b[1]&0x06 != 0:
		return "mp3"
	case bytes.HasPrefix(b, []byte("RIFF")):
		return "wav"
	case bytes.HasPrefix(b, []byte("fLaC")):
		return "flac"
	case len(b) > 1 && b[0] == 0xFF && b[1]&0xF6 == 0xF0:
		return "aac" // ADTS
	}
	ct := strings.ToLower(contentType)
	for _, f := range []struct{ sub, name string }{{"ogg", "ogg"}, {"opus", "ogg"}, {"mpeg", "mp3"}, {"mp3", "mp3"}, {"wav", "wav"}, {"flac", "flac"}, {"aac", "aac"}} {
		if strings.Contains(ct, f.sub) {
			return f.name
		}
	}
	return ""
}

// VoiceContentType is the content type for a VoiceFormat name.
func VoiceContentType(format string) string {
	switch format {
	case "ogg":
		return "audio/ogg"
	case "mp3":
		return "audio/mpeg"
	case "wav":
		return "audio/wav"
	case "flac":
		return "audio/flac"
	case "aac":
		return "audio/aac"
	}
	return "application/octet-stream"
}

// Deletion reports messages someone deleted in a chat app.
type Deletion struct {
	Channel    string
	ChatID     string
	MessageIDs []string
}

// ChatRef is the deletion's channel-qualified chat id.
func (d Deletion) ChatRef() string { return d.Channel + ":" + d.ChatID }

// DeleteWatcher is implemented by channels that hear about deleted
// messages (Discord does; Telegram bots aren't told).
type DeleteWatcher interface {
	OnDelete(func(ctx context.Context, d Deletion))
}

// Bot is the account a chat adapter is signed in as.
type Bot struct {
	Name string
	// URL is where someone adds the bot to a server (Discord) or opens a
	// chat with it (Telegram).
	URL string
}

// Identity is implemented by chat adapters that know their bot account.
type Identity interface {
	// Bot reports the account once the adapter has connected.
	Bot() (Bot, bool)
}

// Checker is implemented by chat adapters that can test their token
// without connecting, so a bad one is refused before anything changes.
type Checker interface {
	Check(ctx context.Context) error
}

// Reaction is an emoji someone added to a message in a chat app.
type Reaction struct {
	Channel   string
	ChatID    string
	MessageID string
	UserID    string // channel-specific user id
	Emoji     string
	IsDM      bool
}

// ChatRef is the reaction's channel-qualified chat id.
func (r Reaction) ChatRef() string { return r.Channel + ":" + r.ChatID }

// ReactionWatcher is implemented by channels that hear about reactions
// (Discord and Telegram), for one-tap commands like "remember this".
type ReactionWatcher interface {
	OnReaction(func(ctx context.Context, r Reaction))
}
