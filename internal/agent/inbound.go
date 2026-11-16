package agent

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/theshield2594/thicket/internal/channel"
	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/llm"
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

	// Credential-shaped, or a value Thicket already holds as a secret
	// (a pasted bot token, say).
	known := e.Secrets.Redact(in.Text) != in.Text
	if !known && !secrets.LooksLikeSecret(in.Text) && !openRouterKeyRe.MatchString(in.Text) {
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
		ready := e.ModelReady(ctx)
		if err := e.Secrets.Put(ctx, secrets.OpenRouterKey, key); err != nil {
			return true, err
		}
		if prev := e.Settings.String(ctx, "model_url"); !ready && e.Settings.IsSet(ctx, "model_url") && !llm.IsOpenRouter(prev) {
			// A local server was tried in chat but never worked: the key
			// means they've chosen OpenRouter.
			for _, k := range []string{"model_url", "model", "cheap_model", "fallback_model"} {
				_ = e.Settings.Set(ctx, k, "")
			}
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
			"Before I can really talk, I need a model to think with. Two ways:\n"+
			"• **OpenRouter** (easiest): make a key at https://openrouter.ai/keys and paste it here. I'll check it works, encrypt it, and never show it to anyone, me included.\n"+
			"• **Your own machine**: if you run Ollama or LM Studio, send me its address (like `http://192.168.1.20:11434`), or just say \"ollama\".")
	}
	return e.send(ctx, in.ChatRef(), host, "I still need a model before I can think properly: paste an OpenRouter key (it starts with `sk-or-`), or send the address of your Ollama or LM Studio server.")
}
