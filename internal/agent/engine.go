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
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/theshield2594/thicket/internal/budget"
	"github.com/theshield2594/thicket/internal/calendar"
	"github.com/theshield2594/thicket/internal/channel"
	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/config"
	"github.com/theshield2594/thicket/internal/llm"
	"github.com/theshield2594/thicket/internal/mcp"
	"github.com/theshield2594/thicket/internal/secrets"
	"github.com/theshield2594/thicket/internal/settings"
	"github.com/theshield2594/thicket/internal/store"
	"github.com/theshield2594/thicket/internal/websearch"
)

// Limits that keep agents from running away.
const (
	maxToolCallsPerTurn  = 8
	maxModelCallsPerTurn = 6
	maxHandoffDepth      = 2
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
	// OAuth signs in to integrations that use OAuth.
	OAuth *mcp.OAuth
	// Calendar is the household calendar.
	Calendar *calendar.Service
	Budget   *budget.Guard
	// Speech reads replies aloud (through the budget guard); nil means
	// sprigs never speak.
	Speech llm.Speaker
	Log    *slog.Logger
	// KeyCheck validates a model API key with the provider.
	KeyCheck func(ctx context.Context, key string) error
	// Fetcher reads web pages for the web tools; nil means one that only
	// reaches public addresses. Tests replace it.
	Fetcher        *websearch.Fetcher
	fetchOnce      sync.Once
	defaultFetcher *websearch.Fetcher
	// ListModels asks a model server which models it has; nil means
	// llm.ListModels.
	ListModels func(ctx context.Context, base, key string) ([]string, error)
	// FetchAttachment downloads a file sent with a message; nil means
	// channel.Fetch. Tests replace it.
	FetchAttachment func(ctx context.Context, a channel.Attachment, max int) ([]byte, string, error)
	Now             func() time.Time
	// Fire starts an event-driven job now (the scheduler's Fire).
	Fire func(store.Job) error

	// ConnectChat connects a chat app ("discord", "telegram") with a bot
	// token while Thicket runs, replacing a connection it already has. It
	// returns once the bot is online; the app sets it.
	ConnectChat func(ctx context.Context, app, token string) error

	chanMu    sync.RWMutex
	channels  map[string]channel.Channel
	chatLocks sync.Map   // chatRef -> *sync.Mutex
	active    sync.Map   // chatRef -> character id
	drafts    sync.Map   // user id -> *character.Character
	kidDrafts sync.Map   // user id -> *kidDraft
	kidMu     sync.Mutex // serialises changes to a kid sprig's file (access, limits, their undo)
	kidTurns  sync.Map   // kid's user id -> *sync.Mutex: one turn at a time per kid
	userMu    sync.Mutex // serialises first-contact user creation
	rateMu    sync.Mutex
	rate      map[int64][]time.Time
	limits    rateLimits // per-sprig and per-integration tool limits
	// compactBackoff: conversation id -> time before which summarising
	// isn't retried (it failed).
	compactBackoff sync.Map
	// Stopping: replies and jobs in progress, messages waiting per chat,
	// and the arrival number up to which a chat's queue was stopped.
	turnsIn     sync.Map // chatRef -> *inFlight
	jobsRunning sync.Map // job id -> *inFlight
	waitingIn   sync.Map // chatRef -> *atomic.Int32
	stopSeq     sync.Map // chatRef -> uint64
	arrivals    atomic.Uint64
	// Background tasks sprigs hand each other (delegate_task). They end
	// with lifetime (set by the app), not with the message that started
	// them.
	tasks    sync.WaitGroup
	taskMu   sync.Mutex
	taskUse  map[int64]int // user id -> background tasks reserved or running
	taskSeq  atomic.Int64
	lifeMu   sync.Mutex
	lifetime context.Context
	activity sync.Map // sprig id -> *busy (for live avatar state)
	// pendingPkgs: user id -> *pendingPackages (sprig packages sent in chat)
	pendingPkgs sync.Map
	// rememberAsks: user id -> rememberAsk ("just for you, or for the
	// family?" waiting for an answer)
	rememberAsks sync.Map
	// costAsks: chatRef -> *costAsk (a costly turn waiting for "go ahead")
	costAsks  sync.Map
	capMu     sync.Mutex // a sprig cap change, or the check and restore of its undo
	linkMu    sync.Mutex
	links     map[string]linkCode    // code -> pending link
	linkFails map[string][]time.Time // channel:user -> failed attempts
}

func (e *Engine) AddChannel(c channel.Channel) {
	if w, ok := c.(channel.DeleteWatcher); ok {
		w.OnDelete(e.HandleDeleted)
	}
	if w, ok := c.(channel.ReactionWatcher); ok {
		w.OnReaction(e.HandleReaction)
	}
	if s, ok := c.(channel.ActionSender); ok {
		s.OnPress(e.HandlePress)
	}
	e.chanMu.Lock()
	defer e.chanMu.Unlock()
	if e.channels == nil {
		e.channels = map[string]channel.Channel{}
	}
	e.channels[c.Name()] = c
}

// RemoveChannel forgets a chat app, if c is still the one registered
// under its name (a chat app connected in chat that didn't come up).
func (e *Engine) RemoveChannel(c channel.Channel) {
	e.chanMu.Lock()
	defer e.chanMu.Unlock()
	if e.channels[c.Name()] == c {
		delete(e.channels, c.Name())
	}
}

// channelFor is the chat app registered under name, or nil. Chat apps can
// be connected while Thicket runs, so the map is only read through here.
func (e *Engine) channelFor(name string) channel.Channel {
	e.chanMu.RLock()
	defer e.chanMu.RUnlock()
	return e.channels[name]
}

// Channels is a copy of the registered chat apps by name.
func (e *Engine) Channels() map[string]channel.Channel {
	e.chanMu.RLock()
	defer e.chanMu.RUnlock()
	out := make(map[string]channel.Channel, len(e.channels))
	for n, c := range e.channels {
		out[n] = c
	}
	return out
}

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

// ErrRateLimited means the user sent too many messages in the last minute.
var ErrRateLimited = errors.New("too many messages in the last minute")

// HandleIncoming is the entry point for every user message from a channel.
func (e *Engine) HandleIncoming(ctx context.Context, in channel.Incoming) {
	if err := e.Handle(ctx, in); err != nil && !errors.Is(err, ErrRateLimited) {
		e.Log.Error("handle message", "err", err, "chat", in.ChatRef())
	}
}

// Handle processes a message and returns once every reply has been sent.
// Replies go out through the channel, as always; the error is for callers
// that answer someone directly (the local API).
func (e *Engine) Handle(ctx context.Context, in channel.Incoming) (err error) {
	defer func() {
		if p := recover(); p != nil {
			e.Log.Error("panic handling message", "panic", p)
			e.logActivity(ctx, "error", "system", "", 0, "panic while handling a message", fmt.Sprint(p))
			err = fmt.Errorf("internal error")
		}
	}()
	err = e.handle(ctx, in)
	if err != nil && !errors.Is(err, ErrRateLimited) {
		e.logActivity(ctx, "error", "system", "", 0, "failed to handle a message", err.Error())
	}
	return err
}

func (e *Engine) handle(ctx context.Context, in channel.Incoming) error {
	addressed, rest := e.addressedCharacter(in.Text)
	// In group channels only answer when spoken to.
	if !in.IsDM && !in.Mentioned && !in.Listening && addressed == nil {
		return nil
	}
	if in.IsDM && in.UserRef == 0 && linkCodeRe.MatchString(in.Text) {
		return e.redeemLinkCode(ctx, in)
	}
	if in.IsDM && in.UserRef == 0 && joinCodeRe.MatchString(in.Text) {
		return e.redeemJoinCode(ctx, in)
	}
	user, err := e.resolveUser(ctx, in)
	if err != nil || user == nil {
		return err
	}
	// Someone else's sprig: as if no sprig was named.
	if addressed != nil && !addressed.UsableBy(user) {
		addressed, rest = nil, in.Text
		if !in.IsDM && !in.Mentioned && !in.Listening {
			return nil
		}
	}
	_ = e.DB.TouchUser(ctx, user.ID)
	// Proactive messages go to the app the user last wrote from (never
	// to an internal channel like the API, which can't receive them), so
	// each chat app account keeps when it was last used (presence.go).
	if in.UserRef == 0 {
		_ = e.DB.UseIdentity(ctx, user.ID, in.Channel, in.UserID)
	}
	// A stop request comes before the rate limit: someone sending a burst
	// of messages is exactly who may need to stop a reply.
	stopText := in.Text
	if addressed != nil {
		stopText = rest // "Pip, stop"
	}
	isStop := isStopRequest(stopText) && len(in.Attachments) == 0
	if isStop {
		stopped, jobs := e.Stop(in.ChatRef(), user)
		if stopped {
			return nil // the reply being stopped says so itself
		}
		if len(jobs) > 0 {
			host, _ := e.Chars.Host()
			return e.send(ctx, in.ChatRef(), host, "Okay, I've stopped "+quoteJoin(jobs)+".")
		}
		// Nothing running: an ordinary message (it may deny an approval).
	}
	if !e.allow(user.ID) {
		e.Log.Warn("rate limited", "user", user.ID)
		return ErrRateLimited
	}
	// Paused: say so at once, and keep nothing for later.
	if err := e.paused(ctx, ""); err != nil {
		if host, ok := e.Chars.Host(); ok {
			return e.send(ctx, in.ChatRef(), host, pausedReply(err))
		}
		return nil
	}
	seq := e.arrivals.Add(1)
	w := e.waiting(in.ChatRef())
	w.Add(1)
	unlock := e.lockChat(in.ChatRef())
	w.Add(-1)
	defer unlock()
	if v, ok := e.stopSeq.Load(in.ChatRef()); ok && seq <= v.(uint64) {
		return nil // sent before a stop: set aside, as the stop said
	}
	ctx, flight, done := e.track(ctx, &e.turnsIn, in.ChatRef(), user.ID, "")
	defer done()
	defer func() {
		// A stop that came after the reply was already out: say so.
		if flight.stopped.Load() && flight.acked.CompareAndSwap(false, true) {
			if host, ok := e.Chars.Host(); ok {
				_ = e.send(context.WithoutCancel(ctx), in.ChatRef(), host, "I'd already finished, so there was nothing left to stop.")
			}
		}
	}()

	// What the user typed, minus a leading sprig name. Attachment markers
	// go after it, so secret and approval checks see them as before.
	typed := in.Text
	if addressed != nil && rest != "" {
		typed = rest
	}
	in.Text = strings.TrimSpace(in.Text + attachmentNotes(in.Attachments))

	host, ok := e.Chars.Host()
	if !ok {
		return errors.New("no host character is loaded; check the characters directory")
	}

	// 1. Secrets never reach the model or the message history.
	if handled, err := e.handleSecrets(ctx, in, user, host); handled || err != nil {
		return err
	}
	// 2. "Go ahead" after a cost question, then answers to a pending approval.
	if handled, err := e.answerCostAsk(ctx, in, user); handled || err != nil {
		return err
	}
	if handled, err := e.handleApprovalReply(ctx, in, user); handled || err != nil {
		return err
	}
	// 3. Pin and delete commands ("pin this", "delete that").
	if handled, err := e.handleMessageCommand(ctx, in, user, typed, addressed); handled || err != nil {
		return err
	}
	// 4. No model yet: scripted first-run setup.
	if !e.ModelReady(ctx) {
		return e.bootstrap(ctx, in, user, host)
	}

	query := strings.TrimSpace(typed + attachmentNotes(in.Attachments))
	// A kid past a time limit gets no reply from the model, so their
	// attachments aren't read either: except voice notes, which are
	// transcribed so the safety check hears what they said.
	if user.IsKid() && len(in.Attachments) > 0 {
		if c, _ := e.replySprig(in, user, addressed); c != nil {
			if why, _ := e.kidLimitReason(ctx, user, c); why != "" {
				in.Attachments = slices.DeleteFunc(slices.Clone(in.Attachments), func(a channel.Attachment) bool { return classify(a) != kindAudio })
			}
		}
	}
	var media loaded
	if len(in.Attachments) > 0 {
		if ch := e.channelFor(in.Channel); ch != nil {
			ch.Typing(ctx, in.ChatID)
		}
		media = e.loadAttachments(ctx, in.Attachments)
		typed += attachmentNotes(in.Attachments) + media.text
	}
	in.Text = strings.TrimSpace(typed)

	char, chosen := e.replySprig(in, user, addressed)
	if char == nil {
		return e.send(ctx, in.ChatRef(), host, "There's no sprig set up for you yet. Ask a grown-up to set one up for you!")
	}

	conv, err := e.DB.OpenConversation(ctx, user.ID, in.ChatRef(), e.Cfg.IdleAfter)
	if err != nil {
		return err
	}
	if !chosen && e.Settings.Bool(ctx, "auto_route") && !user.IsKid() {
		char = e.route(ctx, user, conv, char, in.Text)
	}
	if err := e.paused(ctx, char.ID); err != nil {
		return e.send(ctx, in.ChatRef(), host, pausedReply(err)) // the message isn't kept for later
	}
	e.active.Store(in.ChatRef(), char.ID)
	msgID, err := e.DB.AddMessageID(ctx, conv.ID, "user", "", in.Text)
	if err != nil {
		return err
	}
	if in.MessageID != "" {
		_ = e.DB.AddMessageRefs(ctx, msgID, in.ChatRef(), in.MessageID)
	}
	if ch := e.channelFor(in.Channel); ch != nil {
		ch.Typing(ctx, in.ChatID)
	}
	t := &turn{user: user, char: char, chatRef: in.ChatRef(), conv: conv, isDM: in.IsDM, query: query, images: media.images, pdfs: media.pdfs, pdfPages: media.pdfPages, localDocs: media.localDocs, localText: media.localText,
		packages: media.packages, userMsgID: msgID, spokenTo: media.spoken, said: in.Text}
	if in.NoApprovals {
		t.addTaint(agentSource)
	}
	if user.IsKid() {
		// One turn at a time per kid, across chats and apps, so two at once
		// can't both spend the last of a daily allowance.
		mu, _ := e.kidTurns.LoadOrStore(user.ID, &sync.Mutex{})
		mu.(*sync.Mutex).Lock()
		defer mu.(*sync.Mutex).Unlock()
		// Past a time limit the kid hears so, kindly, and no reply model is
		// called. A message the safety check screens is screened first: if
		// it's flagged (or can't be screened), the kid gets a real answer.
		msg := e.kidLimitReply(ctx, user, char, in.Text)
		if msg != "" && maybeAlert.MatchString(in.Text) {
			concern, summary, failed := e.screen(ctx, user, char, in.Text)
			if concern != "" {
				e.alertParents(context.WithoutCancel(ctx), user, char, concern, summary, strings.TrimSpace(in.Text))
			}
			if concern != "" || failed {
				msg = ""
			}
		} else {
			// After the reply, so the kid isn't kept waiting; never blocks it.
			defer e.safetyCheck(context.WithoutCancel(ctx), user, char, in.Text)
		}
		if msg != "" {
			return e.sayLimit(ctx, in.ChatRef(), conv, char, msg)
		}
	}
	if err := e.respond(ctx, t, ""); err != nil {
		return err
	}
	if user.IsKid() && user.OnboardedAt.IsZero() && t.replied {
		e.greeted(ctx, user) // their kid sprig said its first hello
	}
	return nil
}

// greeted records that a kid's sprig has said its first hello, so it
// happens once (the kid's onboarded_at, as for adults' tours).
func (e *Engine) greeted(ctx context.Context, u *store.User) {
	if err := e.DB.MarkOnboarded(context.WithoutCancel(ctx), u.ID); err != nil {
		e.Log.Warn("mark a kid greeted", "user", u.ID, "err", err)
		return
	}
	u.OnboardedAt = e.now()
}

// replySprig is the sprig that answers this message, before any
// auto-routing: the one named, the web thread's own, the one talking in
// this chat, else the user's default (a kid's own sprig, or the host).
// chosen says the user picked it (by name or by its thread). nil means
// no sprig is set up for this user.
func (e *Engine) replySprig(in channel.Incoming, user *store.User, addressed *character.Character) (char *character.Character, chosen bool) {
	if addressed != nil {
		return addressed, true
	}
	if c, ok := e.Chars.Get(in.Character); ok && in.Character != "" && c.UsableBy(user) {
		return c, true // a per-sprig web thread
	}
	if id, ok := e.active.Load(in.ChatRef()); ok {
		if c, ok := e.Chars.Get(id.(string)); ok && c.UsableBy(user) {
			return c, false
		}
	}
	return e.defaultSprigFor(user), false
}

// respond runs a turn, posts the reply, and follows any handoff.
func (e *Engine) respond(ctx context.Context, t *turn, event string) error {
	ctx = llm.WithMeta(ctx, llm.Meta{UserID: t.user.ID, Character: t.char.ID, Purpose: "chat"})
	reply, err := e.runTurn(ctx, t, event)
	paused := errors.Is(err, ErrPaused)
	if paused {
		err, reply = nil, pausedReply(err)
	}
	var hold *costHold
	if errors.As(err, &hold) {
		// Said in code, and nothing else happens (no PDF is read, nothing
		// summarised) until the person agrees.
		ids, err := e.sendIDs(ctx, t.chatRef, t.char, hold.msg)
		if err == nil && t.conv != nil {
			if id, err := e.DB.AddMessageID(ctx, t.conv.ID, "assistant", t.char.ID, hold.msg); err == nil {
				_ = e.DB.AddMessageRefs(ctx, id, t.chatRef, ids...)
			}
		}
		return err
	}
	if f := inFlightFrom(ctx); f != nil && f.stopped.Load() {
		// Stopped: no reply, no approval prompts, no handoff. What was
		// already done is still said, so it can be undone.
		e.stopAck(ctx, t, f)
		for _, p := range t.afterReply {
			if p.trailID != 0 {
				if err := e.postAction(context.WithoutCancel(ctx), t, t.char, p); err != nil {
					e.Log.Error("say what was done", "err", err)
				}
			}
		}
		return nil
	}
	if err != nil {
		reply = e.failureReply(ctx, t, err)
	}
	var sendErr error
	if reply != "" {
		var ids []string
		var msgID int64
		ids, sendErr = e.sendIDs(ctx, t.chatRef, t.char, reply)
		t.replied = sendErr == nil && err == nil && !paused // a real reply, not a failure or pause notice
		if sendErr == nil && t.conv != nil {
			if id, err := e.DB.AddMessageID(ctx, t.conv.ID, "assistant", t.char.ID, reply); err == nil {
				msgID = id
				_ = e.DB.AddMessageRefs(ctx, id, t.chatRef, ids...)
			}
		}
		// Out loud too, if this sprig speaks here (after the text, which
		// is always sent). Never a failure message. The audio's messages
		// belong to the reply: "delete that" takes them too.
		if sendErr == nil && err == nil {
			if voiceIDs := e.speak(ctx, t, reply, ids); len(voiceIDs) > 0 && msgID != 0 {
				_ = e.DB.AddMessageRefs(ctx, msgID, t.chatRef, voiceIDs...)
			}
		}
	}
	// Approval prompts go out even if the reply failed. An approval is
	// registered only after its prompt was delivered (deliverApproval).
	for _, p := range t.afterReply {
		var err error
		who := t.char
		if p.as != nil {
			who = p.as
		}
		switch {
		case p.approval != nil:
			err = e.deliverApproval(ctx, who, p.approval, p.text)
		case p.trailID != 0:
			err = e.postAction(ctx, t, who, p)
		default:
			err = e.send(ctx, t.chatRef, who, p.text)
		}
		if err != nil {
			e.Log.Error("post after reply", "err", err)
		}
	}
	// Keep the text of PDFs sent this turn for follow-up questions (and
	// for a sprig this is handed to). Even if the reply failed: "try
	// again" should still know the document.
	if len(t.pdfs)+len(t.localDocs) > 0 && t.conv != nil {
		e.keepDocuments(ctx, t)
	}
	// The user has their reply: now's the time to compact a long
	// conversation, before the next message (or a handoff) needs it.
	if t.conv != nil {
		e.maybeCompact(ctx, t)
	}
	if sendErr != nil {
		return sendErr
	}
	if f := inFlightFrom(ctx); f != nil && f.stopped.Load() {
		return nil // stopped after the reply went out: no handoff
	}
	// In the commons, @mentioning another sprig hands over to it.
	if t.handoff == nil && reply != "" && e.isCommons(t.chatRef) {
		if c := e.mentionedSprig(reply, t.char); c != nil && c.UsableBy(t.user) {
			t.handoff = &handoffReq{to: c.ID, message: truncate(reply, 500)}
		}
	}
	if h := t.handoff; h != nil && t.depth < maxHandoffDepth {
		target, ok := e.Chars.Get(h.to)
		if !ok || !target.UsableBy(t.user) {
			return nil
		}
		// A kid sprig past its own time limit doesn't pick it up.
		said := t.said
		if said == "" {
			said = t.query
		}
		if msg := e.kidLimitReply(ctx, t.user, target, said); msg != "" && target.IsKid() {
			e.active.Store(t.chatRef, target.ID)
			return e.sayLimit(ctx, t.chatRef, t.conv, target, msg)
		}
		e.logActivity(ctx, "info", "handoff", t.char.ID, t.user.ID, fmt.Sprintf("%s handed off to %s", t.char.DisplayName(), target.DisplayName()), h.message)
		e.active.Store(t.chatRef, target.ID)
		note := fmt.Sprintf("%s handed this over to %s: %s", t.char.DisplayName(), target.DisplayName(), h.message)
		if t.conv != nil {
			_ = e.DB.AddMessage(ctx, t.conv.ID, "event", "", note)
		}
		next := &turn{user: t.user, char: target, chatRef: t.chatRef, conv: t.conv, isDM: t.isDM, depth: t.depth + 1, query: h.message + " " + t.query, spokenTo: t.spokenTo, said: t.said}
		next.inherit(t)
		next.addTaint("sprig " + t.char.ID) // the handoff note is a sprig's words
		return e.respond(ctx, next, fmt.Sprintf("%s just handed this to you: %q. Pick it up and reply to the user directly.", t.char.DisplayName(), h.message))
	}
	return nil
}

// failureReply turns an error into an honest in-character message, written
// in code (the model may be what's broken).
func (e *Engine) failureReply(ctx context.Context, t *turn, err error) string {
	e.Log.Error("turn failed", "character", t.char.ID, "err", err)
	e.logActivity(ctx, "error", "model", t.char.ID, t.user.ID, "reply failed", err.Error())
	var be *llm.BudgetError
	switch {
	case errors.As(err, &be) && t.user.IsKid():
		return e.kidCapReply(ctx, t, be)
	case errors.As(err, &be):
		return budgetReply(t, be)
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
	if in.UserRef != 0 { // already authenticated by the adapter
		u, err := e.DB.UserByID(ctx, in.UserRef)
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil
		}
		return u, err
	}
	u, err := e.DB.UserByExternal(ctx, in.Channel, in.UserID)
	if err == nil {
		return u, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	// Owner bootstrap is check-then-insert: serialise it so two first DMs
	// can't both become owner, and re-read in case another goroutine won.
	e.userMu.Lock()
	defer e.userMu.Unlock()
	if u, err := e.DB.UserByExternal(ctx, in.Channel, in.UserID); err == nil {
		return u, nil
	}
	hasOwner, err := e.DB.HasOwner(ctx)
	if err != nil {
		return nil, err
	}
	role := ""
	ownerID := e.Cfg.OwnerID(in.Channel)
	noOwnerConfigured := e.Cfg.DiscordOwnerID == "" && e.Cfg.TelegramOwnerID == ""
	switch {
	// A configured owner id makes that account the owner, unless an owner
	// already exists (from another app): then it links like anyone else,
	// so one person never ends up as two owners with split memories.
	case ownerID != "" && in.UserID == ownerID && !hasOwner:
		role = "owner"
	case !hasOwner && noOwnerConfigured && in.IsDM:
		role = "owner"
	case contains(e.Cfg.AllowedUsers, in.UserID) || contains(e.Cfg.AllowedUsers, in.Channel+":"+in.UserID):
		role = "member"
	}
	if role == "" {
		if ok, _ := e.DB.ClaimNotice(ctx, "stranger:"+in.Channel+":"+in.UserID, 24*time.Hour); ok {
			if host, ok := e.Chars.Host(); ok {
				_ = e.send(ctx, in.ChatRef(), host, "Hi! I don't think we've met. This Thicket is private, so ask its owner to invite you and I'll happily say hello properly. "+
					"Already using it on another app? Ask me there for a link code and send it here.")
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

var addressRe = regexp.MustCompile(`(?is)^\s*(?:hey|hi|hello|ok|okay|yo)?[\s,]*@?([a-z][a-z0-9-]*)\s*[,:!\-—]\s*(.*)$`)
var addressBareRe = regexp.MustCompile(`(?is)^\s*(?:hey|hi|hello|yo)[\s,]+@?([a-z][a-z0-9-]*)\b[\s,!.]*(.*)$`)

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
	p := channel.Persona{ID: c.ID, Name: c.DisplayName(), Color: c.ColorInt()}
	switch {
	case strings.HasPrefix(c.Avatar, "https://") || strings.HasPrefix(c.Avatar, "http://"):
		p.AvatarURL = c.Avatar
	case c.Avatar != "" && e.Cfg.PublicURL != "":
		file := filepath.Base(c.Avatar)
		if _, err := os.Stat(filepath.Join(e.Chars.Dir(), file)); err == nil {
			if m := e.moodAvatar(c.ID, file); m != "" {
				file = m // its pleased or unsure picture, while the mood lasts
			}
			p.AvatarURL = e.Cfg.PublicURL + "/avatars/" + file
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
	ch := e.channelFor(name)
	if ch == nil {
		return fmt.Errorf("no channel %q", name)
	}
	return ch.Send(ctx, id, e.persona(c), e.Secrets.Redact(text))
}

// sendIDs is send, also returning the chat app's ids for what was posted
// (none if the channel can't say).
func (e *Engine) sendIDs(ctx context.Context, chatRef string, c *character.Character, text string) ([]string, error) {
	name, id := splitRef(chatRef)
	ch := e.channelFor(name)
	if ch == nil {
		return nil, fmt.Errorf("no channel %q", name)
	}
	text = e.Secrets.Redact(text)
	if p, ok := ch.(channel.Poster); ok {
		return p.SendIDs(ctx, id, e.persona(c), text)
	}
	return nil, ch.Send(ctx, id, e.persona(c), text)
}

// sendActions is send with buttons under the text, on channels that have
// them; elsewhere the text alone goes out. It returns the id of the
// message with the buttons ("" if none went out).
func (e *Engine) sendActions(ctx context.Context, chatRef string, c *character.Character, text string, actions []channel.Action) (string, error) {
	name, id := splitRef(chatRef)
	ch := e.channelFor(name)
	if ch == nil {
		return "", fmt.Errorf("no channel %q", name)
	}
	if s, ok := ch.(channel.ActionSender); ok && len(actions) > 0 {
		ids, err := s.SendActions(ctx, id, e.persona(c), e.Secrets.Redact(text), actions)
		if err != nil || len(ids) == 0 {
			return "", err
		}
		return ids[len(ids)-1], nil
	}
	return "", ch.Send(ctx, id, e.persona(c), e.Secrets.Redact(text))
}

// sendToUser posts proactively in the user's DMs and records it in that
// conversation, so the user can reply with context.
func (e *Engine) sendToUser(ctx context.Context, u *store.User, c *character.Character, text string) error {
	return e.deliverToUser(ctx, u, c, text, true)
}

// sendNotice is sendToUser for operational notices: the user's current
// sprig stays the one they were talking to.
func (e *Engine) sendNotice(ctx context.Context, u *store.User, c *character.Character, text string) error {
	return e.deliverToUser(ctx, u, c, text, false)
}

func (e *Engine) deliverToUser(ctx context.Context, u *store.User, c *character.Character, text string, takeOver bool) error {
	text = e.Secrets.Redact(text)
	// Where the person wants to be reached, falling back to their other
	// apps (presence.go).
	ref, err := e.directTo(ctx, u, c, text)
	if err != nil {
		return err
	}
	if takeOver { // a sprig reaching out, not an operational notice
		defer e.suggestChatApp(ctx, u, ref)
	}
	if takeOver {
		e.active.Store(ref, c.ID)
	}
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
		// Admin notices are failures that need someone: they bypass quiet
		// hours and the daily budget.
		if err := e.sendNotice(ctx, &a, host, text+e.mustReach(ctx, host, &a, "Thicket needs an admin")); err != nil {
			e.Log.Error("notify admin", "user", a.ID, "err", err)
		}
	}
}

func quoteJoin(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = "“" + n + "”"
	}
	return strings.Join(q, ", ")
}

// postAction posts the line saying what a sprig did, keeping its ids so
// "undo" in reply finds the trail entry.
func (e *Engine) postAction(ctx context.Context, t *turn, who *character.Character, p post) error {
	ids, err := e.sendIDs(ctx, t.chatRef, who, p.text)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		ids = []string{fmt.Sprintf("trail-%d", p.trailID)}
	}
	_ = e.DB.AddTrailAcks(ctx, t.chatRef, p.trailID, t.user.ID, ids...)
	if t.conv != nil {
		_ = e.DB.AddMessage(ctx, t.conv.ID, "assistant", who.ID, p.text)
	}
	return nil
}
