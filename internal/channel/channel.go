// Package channel defines the interface every chat adapter implements.
// Discord, Telegram and the web app implement it (plus the internal
// "api" channel of the local HTTP API).
package channel

import "context"

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
