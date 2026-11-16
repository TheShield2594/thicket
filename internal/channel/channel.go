// Package channel defines the interface every chat adapter implements.
// Discord is the first; Telegram slots in behind the same interface.
package channel

import "context"

// Persona is how a character appears in a chat.
type Persona struct {
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
	// Attachments are files sent with the message. Text holds only what
	// the user typed; the engine decides what to download.
	Attachments []Attachment
}

// Attachment is a file sent with a message. URL must be https and is
// fetched with size limits (see Fetch).
type Attachment struct {
	Name        string
	URL         string
	ContentType string // as reported by the platform; may be empty
	Size        int    // bytes, as reported by the platform; 0 if unknown
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
