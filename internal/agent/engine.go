// Package agent is Thicket's core: it routes messages to characters, runs
// the model/tool loop, enforces permissions and approvals, and lets
// characters take initiative.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/theshield2594/thicket/internal/budget"
	"github.com/theshield2594/thicket/internal/channel"
	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/config"
	"github.com/theshield2594/thicket/internal/llm"
	"github.com/theshield2594/thicket/internal/mcp"
	"github.com/theshield2594/thicket/internal/secrets"
	"github.com/theshield2594/thicket/internal/settings"
	"github.com/theshield2594/thicket/internal/store"
)

// Limits that keep agents from running away.
const (
	maxToolCallsPerTurn  = 8
	maxModelCallsPerTurn = 6
	maxHandoffDepth      = 2
	historyMessages      = 30
	userMsgsPerMinute    = 20
	approvalMaxAge       = 30 * time.Minute
	secretCaptureMaxAge  = 15 * time.Minute
)

type Engine struct {
	Cfg      *config.Config
	DB       *store.Store
	LLM      llm.Client
	Chars    *character.Registry
	Settings *settings.Settings
	Secrets  *secrets.Store
	MCP      *mcp.Registry
	Budget   *budget.Guard
	Log      *slog.Logger
	// KeyCheck validates a model API key with the provider.
	KeyCheck func(ctx context.Context, key string) error
	Now      func() time.Time

	channels  map[string]channel.Channel
	chatLocks sync.Map // chatRef -> *sync.Mutex
	active    sync.Map // chatRef -> character id
	drafts    sync.Map // user id -> *character.Character
	rateMu    sync.Mutex
	rate      map[int64][]time.Time
}

func (e *Engine) AddChannel(c channel.Channel) {
	if e.channels == nil {
		e.channels = map[string]channel.Channel{}
	}
	e.channels[c.Name()] = c
}

func (e *Engine) Channels() map[string]channel.Channel { return e.channels }

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Engine) lockChat(ref string) func() {
	v, _ := e.chatLocks.LoadOrStore(ref, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// HandleIncoming is the entry point for every user message from a channel.
func (e *Engine) HandleIncoming(ctx context.Context, in channel.Incoming) {
	defer func() {
		if p := recover(); p != nil {
			e.Log.Error("panic handling message", "panic", p)
			e.logActivity(ctx, "error", "system", "", 0, "panic while handling a message", fmt.Sprint(p))
		}
	}()
	if err := e.handle(ctx, in); err != nil {
		e.Log.Error("handle message", "err", err, "chat", in.ChatRef())
		e.logActivity(ctx, "error", "system", "", 0, "failed to handle a message", err.Error())
	}
}

func (e *Engine) handle(ctx context.Context, in channel.Incoming) error {
	addressed, rest := e.addressedCharacter(in.Text)
	// In group channels only answer when spoken to.
	if !in.IsDM && !in.Mentioned && !in.Listening && addressed == nil {
		return nil
	}
	user, err := e.resolveUser(ctx, in)
	if err != nil || user == nil {
		return err
	}
	_ = e.DB.TouchUser(ctx, user.ID)
	if !e.allow(user.ID) {
		e.Log.Warn("rate limited", "user", user.ID)
		return nil
	}
	unlock := e.lockChat(in.ChatRef())
	defer unlock()

	host, ok := e.Chars.Host()
	if !ok {
		return errors.New("no host character is loaded; check the characters directory")
	}

	// 1. Secrets never reach the model or the message history.
	if handled, err := e.handleSecrets(ctx, in, user, host); handled || err != nil {
		return err
	}
	// 2. Answers to a pending approval.
	if handled, err := e.handleApprovalReply(ctx, in, user); handled || err != nil {
		return err
	}
	// 3. No model key yet: scripted first-run greeting.
	if !e.Secrets.Has(ctx, secrets.OpenRouterKey) {
		return e.bootstrapGreeting(ctx, in, user, host)
	}

	char := host
	if addressed != nil {
		char = addressed
		if rest != "" {
			in.Text = rest
		}
	} else if id, ok := e.active.Load(in.ChatRef()); ok {
		if c, ok := e.Chars.Get(id.(string)); ok {
			char = c
		}
	}
	e.active.Store(in.ChatRef(), char.ID)

	conv, err := e.DB.OpenConversation(ctx, user.ID, in.ChatRef(), e.Cfg.IdleAfter)
	if err != nil {
		return err
	}
	if err := e.DB.AddMessage(ctx, conv.ID, "user", "", in.Text); err != nil {
		return err
	}
	if ch := e.channels[in.Channel]; ch != nil {
		ch.Typing(ctx, in.ChatID)
	}
	t := &turn{user: user, char: char, chatRef: in.ChatRef(), conv: conv, isDM: in.IsDM, query: in.Text}
	return e.respond(ctx, t, "")
}

// respond runs a turn, posts the reply, and follows any handoff.
func (e *Engine) respond(ctx context.Context, t *turn, event string) error {
	ctx = llm.WithMeta(ctx, llm.Meta{UserID: t.user.ID, Character: t.char.ID, Purpose: "chat"})
	reply, err := e.runTurn(ctx, t, event)
	if err != nil {
		reply = e.failureReply(ctx, t, err)
	}
	if reply != "" {
		if err := e.send(ctx, t.chatRef, t.char, reply); err != nil {
			return err
		}
		if t.conv != nil {
			_ = e.DB.AddMessage(ctx, t.conv.ID, "assistant", t.char.ID, reply)
		}
	}
	for _, p := range t.afterReply {
		if err := e.send(ctx, t.chatRef, t.char, p); err != nil {
			e.Log.Error("post after reply", "err", err)
		}
	}
	if h := t.handoff; h != nil && t.depth < maxHandoffDepth {
		target, ok := e.Chars.Get(h.to)
		if !ok {
			return nil
		}
		e.active.Store(t.chatRef, target.ID)
		note := fmt.Sprintf("%s handed this over to %s: %s", t.char.DisplayName(), target.DisplayName(), h.message)
		if t.conv != nil {
			_ = e.DB.AddMessage(ctx, t.conv.ID, "event", "", note)
		}
		next := &turn{user: t.user, char: target, chatRef: t.chatRef, conv: t.conv, isDM: t.isDM, depth: t.depth + 1, query: h.message + " " + t.query}
		return e.respond(ctx, next, fmt.Sprintf("%s just handed this to you: %q. Pick it up and reply to the user directly.", t.char.DisplayName(), h.message))
	}
	return nil
}

// failureReply turns an error into an honest in-character message, written
// in code (the model may be what's broken).
func (e *Engine) failureReply(ctx context.Context, t *turn, err error) string {
	e.Log.Error("turn failed", "character", t.char.ID, "err", err)
	e.logActivity(ctx, "error", "model", t.char.ID, t.user.ID, "reply failed", err.Error())
	switch {
	case llm.IsBudget(err):
		return "I've hit the spending cap, so I'm going quiet to save your wallet. You can raise it by telling me a new daily or monthly cap."
	case errors.Is(err, llm.ErrNoKey):
		return "I can't reach my brain right now: there's no model API key set up. DM me your OpenRouter key and I'll be back."
	case llm.Fatal(err):
		return "The model provider turned me away (" + shortErr(err) + "). It might be the API key or credits running out. Can you check OpenRouter?"
	default:
		return "Something went wrong on my end and I couldn't finish that (" + shortErr(err) + "). Try again in a moment?"
	}
}

func shortErr(err error) string {
	s := err.Error()
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

// resolveUser maps a channel user to a Thicket user, creating the owner on
// first contact and politely turning away strangers.
func (e *Engine) resolveUser(ctx context.Context, in channel.Incoming) (*store.User, error) {
	u, err := e.DB.UserByExternal(ctx, in.Channel, in.UserID)
	if err == nil {
		return u, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	hasOwner, err := e.DB.HasOwner(ctx)
	if err != nil {
		return nil, err
	}
	role := ""
	switch {
	case in.Channel == "discord" && e.Cfg.DiscordOwnerID != "" && in.UserID == e.Cfg.DiscordOwnerID:
		role = "owner"
	case !hasOwner && e.Cfg.DiscordOwnerID == "" && in.IsDM:
		role = "owner"
	case contains(e.Cfg.AllowedUsers, in.UserID):
		role = "member"
	}
	if role == "" {
		if ok, _ := e.DB.ClaimNotice(ctx, "stranger:"+in.Channel+":"+in.UserID, 24*time.Hour); ok {
			if host, ok := e.Chars.Host(); ok {
				_ = e.send(ctx, in.ChatRef(), host, "Hi! I don't think we've met. This Thicket is private, so ask its owner to invite you and I'll happily say hello properly.")
			}
		}
		return nil, nil
	}
	u, err = e.DB.CreateUser(ctx, in.Channel, in.UserID, "", role)
	if err != nil {
		return nil, err
	}
	e.Log.Info("new user", "id", u.ID, "role", role)
	e.logActivity(ctx, "info", "system", "", u.ID, "new "+role+" joined", in.UserName)
	return u, nil
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// allow applies the per-user message rate limit.
func (e *Engine) allow(userID int64) bool {
	e.rateMu.Lock()
	defer e.rateMu.Unlock()
	if e.rate == nil {
		e.rate = map[int64][]time.Time{}
	}
	now := e.now()
	recent := e.rate[userID][:0]
	for _, t := range e.rate[userID] {
		if now.Sub(t) < time.Minute {
			recent = append(recent, t)
		}
	}
	if len(recent) >= userMsgsPerMinute {
		e.rate[userID] = recent
		return false
	}
	e.rate[userID] = append(recent, now)
	return true
}

var addressRe = regexp.MustCompile(`(?i)^\s*(?:hey|hi|hello|ok|okay|yo)?[\s,]*@?([a-z][a-z0-9-]*)\s*[,:!\-—]\s*(.*)$`)
var addressBareRe = regexp.MustCompile(`(?i)^\s*(?:hey|hi|hello|yo)[\s,]+@?([a-z][a-z0-9-]*)\b[\s,!.]*(.*)$`)

// addressedCharacter detects "Pip, ..." / "hey Juni ..." at the start of a message.
func (e *Engine) addressedCharacter(text string) (*character.Character, string) {
	for _, re := range []*regexp.Regexp{addressRe, addressBareRe} {
		if m := re.FindStringSubmatch(text); m != nil {
			if c, ok := e.Chars.FindByName(m[1]); ok {
				return c, strings.TrimSpace(m[2])
			}
		}
	}
	return nil, text
}

// persona is how a character appears in chat.
func (e *Engine) persona(c *character.Character) channel.Persona {
	p := channel.Persona{Name: c.DisplayName(), Color: c.ColorInt()}
	switch {
	case strings.HasPrefix(c.Avatar, "https://") || strings.HasPrefix(c.Avatar, "http://"):
		p.AvatarURL = c.Avatar
	case c.Avatar != "" && e.Cfg.PublicURL != "":
		if _, err := os.Stat(filepath.Join(e.Chars.Dir(), filepath.Base(c.Avatar))); err == nil {
			p.AvatarURL = e.Cfg.PublicURL + "/avatars/" + filepath.Base(c.Avatar)
		}
	}
	return p
}

func splitRef(ref string) (string, string) {
	ch, id, _ := strings.Cut(ref, ":")
	return ch, id
}

// send posts text to a chat as a character.
func (e *Engine) send(ctx context.Context, chatRef string, c *character.Character, text string) error {
	name, id := splitRef(chatRef)
	ch := e.channels[name]
	if ch == nil {
		return fmt.Errorf("no channel %q", name)
	}
	return ch.Send(ctx, id, e.persona(c), text)
}

// sendToUser posts proactively in the user's DMs and records it in that
// conversation, so the user can reply with context.
func (e *Engine) sendToUser(ctx context.Context, u *store.User, c *character.Character, text string) error {
	ch := e.channels[u.Channel]
	if ch == nil {
		return fmt.Errorf("no channel %q for user %d", u.Channel, u.ID)
	}
	chatID, err := ch.SendDirect(ctx, u.ExternalID, e.persona(c), text)
	if err != nil {
		return err
	}
	ref := u.Channel + ":" + chatID
	e.active.Store(ref, c.ID)
	conv, err := e.DB.OpenConversation(ctx, u.ID, ref, e.Cfg.IdleAfter)
	if err != nil {
		return err
	}
	return e.DB.AddMessage(ctx, conv.ID, "assistant", c.ID, text)
}

func (e *Engine) logActivity(ctx context.Context, level, kind, char string, userID int64, summary, detail string) {
	if err := e.DB.LogActivity(ctx, store.Activity{Level: level, Kind: kind, Character: char, UserID: userID, Summary: summary, Detail: detail}); err != nil {
		e.Log.Error("log activity", "err", err)
	}
}

// Notify tells every admin about something, in the host's voice, at most
// once per window for the same key. This is how Thicket fails loudly.
func (e *Engine) Notify(ctx context.Context, key string, window time.Duration, level, text string) {
	e.logActivity(ctx, level, "system", "", 0, text, "")
	if ok, err := e.DB.ClaimNotice(ctx, "notify:"+key, window); err != nil || !ok {
		return
	}
	host, ok := e.Chars.Host()
	if !ok {
		e.Log.Error("cannot notify: no host character", "text", text)
		return
	}
	admins, err := e.DB.Admins(ctx)
	if err != nil {
		e.Log.Error("list admins", "err", err)
		return
	}
	for _, a := range admins {
		a := a
		if err := e.sendToUser(ctx, &a, host, text); err != nil {
			e.Log.Error("notify admin", "user", a.ID, "err", err)
		}
	}
}
