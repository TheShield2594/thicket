package agent

import (
	"context"
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

var openRouterKeyRe = regexp.MustCompile(`sk-or-[A-Za-z0-9_\-]{20,}`)

// handleSecrets intercepts anything secret-shaped before it can reach the
// model or the message history.
func (e *Engine) handleSecrets(ctx context.Context, in channel.Incoming, user *store.User, host *character.Character) (bool, error) {
	pending, err := e.DB.PendingSecretFor(ctx, user.ID, secretCaptureMaxAge)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return false, err
	}
	if pending != nil {
		if !in.IsDM {
			if secrets.LooksLikeSecret(in.Text) {
				e.scrub(ctx, in)
			}
			return true, e.send(ctx, in.ChatRef(), host, "Let's keep that private: send it to me in a DM instead.")
		}
		if strings.EqualFold(strings.TrimSpace(in.Text), "cancel") {
			_ = e.DB.ClearPendingSecret(ctx, user.ID)
			return true, e.send(ctx, in.ChatRef(), host, "No problem, I've stopped waiting for it.")
		}
		value := strings.TrimSpace(in.Text)
		if strings.ContainsAny(value, " \n") && !secrets.LooksLikeSecret(value) {
			return true, e.send(ctx, in.ChatRef(), host, fmt.Sprintf("That doesn't look like a key or token. Send just the %s on its own, or say **cancel**.", pending.Purpose))
		}
		if err := e.Secrets.Put(ctx, pending.Name, value); err != nil {
			return true, err
		}
		_ = e.DB.ClearPendingSecret(ctx, user.ID)
		e.scrub(ctx, in)
		e.logActivity(ctx, "info", "system", "", user.ID, "secret "+pending.Name+" stored", "")
		return true, e.continueAfterEvent(ctx, in, user, host,
			fmt.Sprintf("The user sent the secret for %s. Thicket stored it encrypted as {{secret:%s}} (you can't see the value). Carry on with the setup.", pending.Purpose, pending.Name))
	}

	if !secrets.LooksLikeSecret(in.Text) && !openRouterKeyRe.MatchString(in.Text) {
		return false, nil
	}
	if !in.IsDM {
		e.scrub(ctx, in)
		return true, e.send(ctx, in.ChatRef(), host, "I deleted that message because it looked like a password or API key. Please don't post those in shared channels. If you need to give me one, DM me.")
	}
	if key := openRouterKeyRe.FindString(in.Text); key != "" && user.IsAdmin() {
		if e.KeyCheck != nil {
			cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			err := e.KeyCheck(cctx, key)
			cancel()
			if err != nil {
				return true, e.send(ctx, in.ChatRef(), host, "Hmm, OpenRouter didn't accept that key ("+shortErr(err)+"). Could you double-check it and send it again?")
			}
		}
		if err := e.Secrets.Put(ctx, secrets.OpenRouterKey, key); err != nil {
			return true, err
		}
		e.scrub(ctx, in)
		e.logActivity(ctx, "info", "system", "", user.ID, "model API key connected", "")
		_ = e.send(ctx, in.ChatRef(), host, "Got it, and it works! I've locked it away encrypted. Nobody, me included, gets to see it again.")
		return true, e.continueAfterEvent(ctx, in, user, host,
			"The user just connected their OpenRouter API key (it's stored safely; don't mention the key itself). Continue onboarding from where things stand.")
	}
	return true, e.send(ctx, in.ChatRef(), host, "That looks like a key or password, so I didn't read it or save it. If you want me to use it, tell me what it's for and I'll ask for it properly.")
}

// scrub deletes a message containing a secret, best effort (Discord won't
// let bots delete user messages in DMs).
func (e *Engine) scrub(ctx context.Context, in channel.Incoming) {
	if ch := e.channels[in.Channel]; ch != nil && in.MessageID != "" {
		if err := ch.Delete(ctx, in.ChatID, in.MessageID); err != nil {
			e.Log.Debug("couldn't delete secret message", "err", err)
		}
	}
}

func (e *Engine) continueAfterEvent(ctx context.Context, in channel.Incoming, user *store.User, char *character.Character, event string) error {
	conv, err := e.DB.OpenConversation(ctx, user.ID, in.ChatRef(), e.Cfg.IdleAfter)
	if err != nil {
		return err
	}
	_ = e.DB.AddMessage(ctx, conv.ID, "event", "", event)
	e.active.Store(in.ChatRef(), char.ID)
	return e.respond(ctx, &turn{user: user, char: char, chatRef: in.ChatRef(), conv: conv, isDM: in.IsDM}, "")
}

var (
	approveRe = regexp.MustCompile(`(?i)^\s*(approve|approved|yes|y|yep|yeah|ok|okay|go ahead|do it|confirm)[\s.!]*$`)
	denyRe    = regexp.MustCompile(`(?i)^\s*(deny|denied|no|n|nope|cancel|stop|don'?t)[\s.!]*$`)
)

// handleApprovalReply resolves a pending risky action.
func (e *Engine) handleApprovalReply(ctx context.Context, in channel.Incoming, user *store.User) (bool, error) {
	approve, deny := approveRe.MatchString(in.Text), denyRe.MatchString(in.Text)
	if !approve && !deny {
		return false, nil
	}
	a, err := e.DB.LatestPendingApproval(ctx, user.ID, in.ChatRef(), approvalMaxAge)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	status := "approved"
	if deny {
		status = "denied"
	}
	if ok, err := e.DB.DecideApproval(ctx, a.ID, status); err != nil || !ok {
		return false, err
	}
	char, ok := e.Chars.Get(a.Character)
	if !ok {
		char, _ = e.Chars.Host()
	}
	conv, err := e.DB.OpenConversation(ctx, user.ID, in.ChatRef(), e.Cfg.IdleAfter)
	if err != nil {
		return true, err
	}
	_ = e.DB.AddMessage(ctx, conv.ID, "user", "", in.Text)
	e.logActivity(ctx, "info", "approval", char.ID, user.ID, fmt.Sprintf("approval #%d %s (%s)", a.ID, status, a.Tool), a.Args)
	t := &turn{user: user, char: char, chatRef: in.ChatRef(), conv: conv, isDM: in.IsDM}
	if deny {
		return true, e.respond(ctx, t, fmt.Sprintf("The user said no to running %s. Acknowledge briefly and don't try it again unless they ask.", a.Tool))
	}
	tool := e.toolsFor(ctx, t)[a.Tool]
	var result string
	if tool == nil {
		result = "Error: that tool is no longer available to you."
	} else {
		out, err := tool.Run(ctx, t, json.RawMessage(a.Args))
		if err != nil {
			out = "Error: " + err.Error()
			e.logActivity(ctx, "warn", "tool", char.ID, user.ID, "approved tool "+a.Tool+" failed", err.Error())
		} else {
			e.logActivity(ctx, "info", "tool", char.ID, user.ID, "ran approved tool "+a.Tool, e.Secrets.Redact(truncate(out, 500)))
		}
		// Redact before anything else sees it: the model, the DB, the chat.
		out = e.Secrets.Redact(out)
		result = out
		if tool.Untrusted {
			result = wrapUntrusted(a.Tool, out)
		}
	}
	_ = e.DB.SetApprovalResult(ctx, a.ID, truncate(result, 2000))
	return true, e.respond(ctx, t, fmt.Sprintf("The user approved running %s. Result:\n%s\nTell them how it went, honestly.", a.Tool, result))
}

// bootstrapGreeting handles messages before any model key exists. It is
// scripted: there's no model to talk with yet.
func (e *Engine) bootstrapGreeting(ctx context.Context, in channel.Incoming, user *store.User, host *character.Character) error {
	if !user.IsAdmin() {
		if ok, _ := e.DB.ClaimNotice(ctx, fmt.Sprintf("bootstrap-member:%d", user.ID), 6*time.Hour); ok {
			return e.send(ctx, in.ChatRef(), host, "Hi! I'm "+host.DisplayName()+". I'm still being set up, so I can't chat properly yet. Check back soon!")
		}
		return nil
	}
	if !in.IsDM {
		return e.send(ctx, in.ChatRef(), host, "Hi! I'm "+host.DisplayName()+". Send me a DM and we'll get you set up.")
	}
	if ok, _ := e.DB.ClaimNotice(ctx, fmt.Sprintf("bootstrap-greet:%d", user.ID), time.Hour); ok {
		return e.send(ctx, in.ChatRef(), host, "Hi! I'm "+host.DisplayName()+", the gardener around here. 🌱 Welcome to your Thicket.\n\n"+
			"Before I can really talk, I need a model key to think with. Make one at https://openrouter.ai/keys and paste it here in this DM. "+
			"I'll check it works, encrypt it, and never show it to anyone, me included.")
	}
	return e.send(ctx, in.ChatRef(), host, "I'm still waiting on that OpenRouter key before I can think properly. Paste it here whenever you're ready (it starts with `sk-or-`).")
}
