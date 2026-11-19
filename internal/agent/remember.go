package agent

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/theshield2594/thicket/internal/channel"
	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/memory"
	"github.com/theshield2594/thicket/internal/secrets"
	"github.com/theshield2594/thicket/internal/store"
)

// "Remember this", handled in code like "pin this": the message becomes a
// locked memory the person set (source user), without depending on the
// model deciding to call remember.
//
//   - "remember this" as a reply keeps that message; without a reply,
//     "remember this/that" keeps the newest message in the conversation.
//   - "remember this: <text>" keeps the text.
//   - A reaction (🧠 or 📌 on Discord, ✍ on Telegram, where 🧠 isn't
//     one of the reactions a bot can see) and the web app's button do
//     the same for the message they're on.
//   - The sprig confirms in one line; "forget this" in reply to that
//     line undoes it.
//   - In a family sprig's chat an adult is asked once: "just for you, or
//     for the family?". The answer sticks; "remember this for the
//     family" or "… just for me" overrides it for one memory.
//
// A pin keeps a message in view for one conversation; a memory lasts.

var (
	rememberScope   = `(?:\s+(?P<scope>(?:just\s+)?for\s+(?:me|myself|us)|just\s+(?:me|us)|for\s+(?:the\s+)?(?:whole\s+)?(?:family|household|everyone)))?`
	rememberRe      = regexp.MustCompile(`(?i)^\s*(?:please\s+)?remember\s+(?:this|that|it)` + rememberScope + `(?:[\s,]+please)?[\s.!]*$`)
	rememberTextRe  = regexp.MustCompile(`(?is)^\s*(?:please\s+)?remember(?:\s+this)?` + rememberScope + `\s*[:–—]\s*(?P<text>\S.*)$`)
	familyAnswerRe  = regexp.MustCompile(`(?i)^\s*(?:(?:for|with)\s+)?(?:the\s+)?(?:whole\s+)?(?:family|household|everyone|all\s+of\s+us)[\s.!]*$`)
	privateAnswerRe = regexp.MustCompile(`(?i)^\s*(?:just\s+)?(?:for\s+)?(?:me|myself|mine|us|just\s+us|just\s+you|private|only\s+me)[\s.!]*$`)
)

// rememberEmoji are the reactions that mean "remember this".
var rememberEmoji = map[string]bool{"🧠": true, "📌": true, "✍": true}

// rememberAskFor is how long "just for you, or for the family?" waits.
const rememberAskFor = 30 * time.Minute

type rememberAsk struct {
	memoryID int64
	chatRef  string
	at       time.Time
}

// rememberCommand parses "remember this" commands. ok is false for
// anything else; text is what to keep when it came with the command.
func rememberCommand(s string) (ok bool, scope, text string) {
	if m := rememberRe.FindStringSubmatch(s); m != nil {
		return true, scopeWord(m[rememberRe.SubexpIndex("scope")]), ""
	}
	if m := rememberTextRe.FindStringSubmatch(s); m != nil {
		return true, scopeWord(m[rememberTextRe.SubexpIndex("scope")]), strings.TrimSpace(m[rememberTextRe.SubexpIndex("text")])
	}
	return false, "", ""
}

func scopeWord(s string) string {
	s = strings.ToLower(s)
	switch {
	case s == "":
		return store.RememberUnset
	case strings.Contains(s, "famil") || strings.Contains(s, "household") || strings.Contains(s, "everyone"):
		return store.RememberFamily
	}
	return store.RememberPrivate
}

// sprigForChat is the sprig a chat is with: a web app sprig thread's own
// sprig, else the one currently talking there.
func (e *Engine) sprigForChat(chatRef string) *character.Character {
	if name, id := splitRef(chatRef); name == "web" {
		if _, sprig, ok := strings.Cut(id, "."); ok {
			if c, ok := e.Chars.Get(sprig); ok {
				return c
			}
		}
	}
	return e.chatSprig(chatRef)
}

// rememberMessageCommand handles "remember this" (and an answer to "just
// for you, or for the family?"). handled is false for anything else.
func (e *Engine) rememberMessageCommand(ctx context.Context, in channel.Incoming, user *store.User, text string, who *character.Character) (handled bool, ack string, memID int64, err error) {
	if ack, handled, err := e.answerRememberAsk(ctx, in.ChatRef(), user, text); handled || err != nil {
		return true, ack, 0, err
	}
	ok, scope, words := rememberCommand(text)
	if !ok || len(in.Attachments) > 0 {
		return false, "", 0, nil
	}
	if words != "" {
		memID, ack := e.remember(ctx, in.ChatRef(), user, who, saidBy(user.Name, "")+quoteFull(words), scope)
		return true, ack, memID, nil
	}
	var target *store.Message
	if in.ReplyTo != "" {
		m, msg, err := e.repliedMessage(ctx, in, user)
		if m == nil {
			return true, msg, 0, err
		}
		target = &m.Message
	} else {
		conv, err := e.DB.CurrentConversation(ctx, user.ID, in.ChatRef(), e.Cfg.IdleAfter)
		if errors.Is(err, store.ErrNotFound) {
			return true, "There's nothing in our conversation to remember yet. Reply to a message with \"remember this\", or say \"remember this: …\" with what to keep.", 0, nil
		}
		if err != nil {
			return true, "", 0, err
		}
		if target, err = e.DB.LastMessage(ctx, conv.ID, "user", "assistant"); errors.Is(err, store.ErrNotFound) {
			return true, "There's nothing in our conversation to remember yet.", 0, nil
		} else if err != nil {
			return true, "", 0, err
		}
	}
	content, refuse := e.messageMemory(user, target)
	if refuse != "" {
		return true, refuse, 0, nil
	}
	memID, ack = e.remember(ctx, in.ChatRef(), user, who, content, scope)
	return true, ack, memID, nil
}

// messageMemory is how a message reads as a memory: who said it, and
// what. refuse explains why a message can't be one.
func (e *Engine) messageMemory(user *store.User, m *store.Message) (content, refuse string) {
	switch m.Role {
	case "user":
		return saidBy(user.Name, "") + quoteFull(m.Content), ""
	case "assistant":
		return saidBy("", e.charName(m.Character)) + quoteFull(m.Content), ""
	case "document":
		return "", "That's a file, which is too long to keep as a memory. Tell me what about it to remember (\"remember this: …\")."
	}
	return "", "That's one of Thicket's own notes, which I can't keep as a memory."
}

func saidBy(person, sprig string) string {
	switch {
	case sprig != "":
		return sprig + " said: "
	case person != "":
		return person + " said: "
	}
	return "They said: "
}

// quoteFull quotes a message, cut to fit a memory.
func quoteFull(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if max := memory.MaxContent - 40; utf8.RuneCountInString(s) > max {
		s = string([]rune(s)[:max]) + "…"
	}
	return "“" + s + "”"
}

// remember keeps content as a locked memory and returns its id (0 if
// nothing was saved) and the line the sprig confirms with.
func (e *Engine) remember(ctx context.Context, chatRef string, user *store.User, who *character.Character, content, scope string) (int64, string) {
	id, ack, err := e.keepMemory(ctx, chatRef, user, who, content, scope)
	if err != nil {
		e.Log.Error("remember this", "err", err)
		return 0, "I couldn't save that just now. Try again in a moment."
	}
	return id, ack
}

// keepMemory saves a locked memory the person set and returns its id and
// the confirmation. id is 0 when nothing was saved.
func (e *Engine) keepMemory(ctx context.Context, chatRef string, user *store.User, who *character.Character, content, scope string) (int64, string, error) {
	if secrets.LooksLikeSecret(content) {
		return 0, "That looks like a password or key, so I won't keep it as a memory. To store a secret, just paste it and I'll keep it safe.", nil
	}
	family, ask := false, false
	if who != nil && who.Tier() == character.TierFamily && !user.IsKid() {
		saved, err := e.DB.RememberScope(ctx, user.ID)
		if err != nil {
			return 0, "", err
		}
		switch {
		case scope != store.RememberUnset:
			family = scope == store.RememberFamily
		case saved != store.RememberUnset:
			family = saved == store.RememberFamily
		default:
			ask, _ = e.DB.ClaimNotice(ctx, fmt.Sprintf("remember-scope:%d", user.ID), 100*365*24*time.Hour)
		}
	}
	existing, err := e.DB.AllMemories(ctx, user.ID)
	if err != nil {
		return 0, "", err
	}
	if family {
		fam, err := e.DB.FamilyMemories(ctx, "")
		if err != nil {
			return 0, "", err
		}
		existing = append(existing, fam...)
	}
	for _, m := range existing {
		if m.Character != "" || m.Household != family || memory.Similarity(m.Content, content) < 0.75 {
			continue
		}
		if !m.Locked {
			if err := e.DB.LockMemory(ctx, m.ID); err != nil {
				return 0, "", err
			}
		}
		e.logActivity(ctx, "info", "memory", "", user.ID, "kept a memory the user asked to remember (already known)", "")
		return m.ID, "🧠 I already had that: " + quotePreview(m.Content) + ". It's yours now, so I won't change it on my own.", nil
	}
	m := store.Memory{UserID: user.ID, Kind: "fact", Content: content, Source: "user", Locked: true, Household: family}
	ids, err := e.DB.ApplyMemoryChanges(ctx, 0, []store.MemoryChange{{Action: "add", Memory: m}})
	if err != nil {
		return 0, "", err
	}
	e.logActivity(ctx, "info", "memory", "", user.ID, "remembered a message the user asked to keep", "")
	ack := "🧠 Remembered: " + quotePreview(content) + "."
	if family {
		ack = "🧠 Remembered for the family: " + quotePreview(content) + "."
	}
	if ask {
		e.rememberAsks.Store(user.ID, rememberAsk{memoryID: ids[0], chatRef: chatRef, at: e.now()})
		ack += " Just for you, or for the family?"
	}
	return ids[0], ack, nil
}

// answerRememberAsk takes the answer to "just for you, or for the
// family?". Anything else drops the question.
func (e *Engine) answerRememberAsk(ctx context.Context, chatRef string, user *store.User, text string) (string, bool, error) {
	v, ok := e.rememberAsks.LoadAndDelete(user.ID)
	if !ok {
		return "", false, nil
	}
	a := v.(rememberAsk)
	if a.chatRef != chatRef || e.now().Sub(a.at) > rememberAskFor {
		return "", false, nil
	}
	switch {
	case familyAnswerRe.MatchString(text):
		if err := e.DB.SetMemoryHousehold(ctx, a.memoryID, true); err != nil {
			return "", true, err
		}
		if err := e.DB.SetRememberScope(ctx, user.ID, store.RememberFamily); err != nil {
			return "", true, err
		}
		return "Shared it with the family. I'll do the same next time; say \"remember this just for me\" when it's yours alone.", true, nil
	case privateAnswerRe.MatchString(text):
		if err := e.DB.SetRememberScope(ctx, user.ID, store.RememberPrivate); err != nil {
			return "", true, err
		}
		return "Kept it just for you. I'll do the same next time; say \"remember this for the family\" when it's everyone's.", true, nil
	}
	return "", false, nil
}

// forgetRemembered undoes a "remember this" when "forget this" replies to
// its confirmation. handled is false if the reply isn't to one.
func (e *Engine) forgetRemembered(ctx context.Context, in channel.Incoming, user *store.User) (string, bool, error) {
	memID, owner, err := e.DB.RememberAck(ctx, in.ChatRef(), in.ReplyTo)
	if errors.Is(err, store.ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", true, err
	}
	if owner != user.ID {
		return "That memory is someone else's, so I'll leave it be.", true, nil
	}
	m, err := e.DB.MemoryByID(ctx, memID)
	if errors.Is(err, store.ErrNotFound) {
		return "That one's already forgotten.", true, nil
	}
	if err != nil {
		return "", true, err
	}
	if m.Household && user.IsKid() {
		return "That's family memory, so a grown-up would have to forget it.", true, nil
	}
	if _, err := e.DB.ApplyMemoryChanges(ctx, 0, []store.MemoryChange{{Action: "delete", Memory: store.Memory{ID: memID}}}); err != nil {
		return "", true, err
	}
	e.logActivity(ctx, "info", "memory", "", user.ID, "forgot a memory the user had asked to remember", "")
	return "Forgotten: " + quotePreview(m.Content) + ".", true, nil
}

// confirmRemember posts the sprig's confirmation and records it against
// the memory, so "forget this" in reply to it can undo it.
func (e *Engine) confirmRemember(ctx context.Context, chatRef string, user *store.User, who *character.Character, ack string, memID int64) error {
	ids, err := e.sendIDs(ctx, chatRef, who, ack)
	if err != nil || memID == 0 {
		return err
	}
	return e.DB.AddRememberAcks(ctx, chatRef, memID, user.ID, ids...)
}

// HandleReaction runs one-tap commands sent as reactions: 🧠, 📌 or ✍ on a
// message remembers it.
func (e *Engine) HandleReaction(ctx context.Context, r channel.Reaction) {
	if !rememberEmoji[strings.TrimSuffix(r.Emoji, "️")] {
		return
	}
	user, err := e.DB.UserByExternal(ctx, r.Channel, r.UserID)
	if err != nil {
		return // a stranger, or someone not set up yet
	}
	if e.paused(ctx, "") != nil {
		return
	}
	in := channel.Incoming{Channel: r.Channel, ChatID: r.ChatID, UserID: r.UserID, ReplyTo: r.MessageID, IsDM: r.IsDM}
	unlock := e.lockChat(in.ChatRef())
	defer unlock()
	ack, memID, err := e.rememberReplied(ctx, in, user)
	if err != nil {
		e.Log.Error("remember by reaction", "err", err)
		return
	}
	if ack == "" {
		return
	}
	if who := e.sprigForChat(in.ChatRef()); who != nil {
		if err := e.confirmRemember(ctx, in.ChatRef(), user, who, ack, memID); err != nil {
			e.Log.Error("confirm remember", "err", err)
		}
	}
}

// rememberReplied remembers the message in.ReplyTo points at. It says
// nothing about a message it doesn't know in a group chat (people react
// to all sorts there).
func (e *Engine) rememberReplied(ctx context.Context, in channel.Incoming, user *store.User) (string, int64, error) {
	m, err := e.DB.MessageByRef(ctx, in.ChatRef(), in.ReplyTo)
	if errors.Is(err, store.ErrNotFound) {
		if !in.IsDM {
			return "", 0, nil
		}
		return "I don't have that message in our conversation anymore, so I can't remember it. Say \"remember this: …\" with what to keep.", 0, nil
	}
	if err != nil {
		return "", 0, err
	}
	if m.UserID != user.ID {
		return "", 0, nil // someone else's conversation: not theirs to keep
	}
	content, refuse := e.messageMemory(user, &m.Message)
	if refuse != "" {
		return refuse, 0, nil
	}
	memID, ack := e.remember(ctx, in.ChatRef(), user, e.sprigForChat(in.ChatRef()), content, store.RememberUnset)
	return ack, memID, nil
}

// RememberWebMessage remembers a web app message for its user (the web
// app's remember button) and returns what the sprig said about it.
func (e *Engine) RememberWebMessage(ctx context.Context, userID int64, chatRef, platformID string) (string, error) {
	user, err := e.DB.UserByID(ctx, userID)
	if err != nil {
		return "", err
	}
	name, chatID := splitRef(chatRef)
	in := channel.Incoming{Channel: name, ChatID: chatID, ReplyTo: platformID, IsDM: true}
	unlock := e.lockChat(chatRef)
	defer unlock()
	ack, memID, err := e.rememberReplied(ctx, in, user)
	if err != nil || ack == "" {
		return ack, err
	}
	if who := e.sprigForChat(chatRef); who != nil {
		if err := e.confirmRemember(ctx, chatRef, user, who, ack, memID); err != nil {
			return "", err
		}
	}
	return ack, nil
}
