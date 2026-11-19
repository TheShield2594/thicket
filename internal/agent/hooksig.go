package agent

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/store"
)

// Webhook signatures. By default the URL's token is a webhook's only
// secret. A webhook can also require the sender to sign each call with a
// shared secret, the way GitHub, Gitea/Forgejo and GitLab do, so a leaked
// URL alone can't start it. The secret lives in the encrypted secret
// store (checking an HMAC needs it) and reaches the user by a DM written
// by code.

var hookStyles = map[string]string{
	"github": "GitHub",
	"gitea":  "Gitea or Forgejo",
	"gitlab": "GitLab",
}

var (
	ErrBadSignature = errors.New("missing or wrong signature")
	ErrDuplicate    = errors.New("already received")
)

func hookSecretName(jobID int64) string { return fmt.Sprintf("webhook_%d_signing", jobID) }

func newHookSecret() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "thk_whs_" + base64.RawURLEncoding.EncodeToString(b), nil
}

func hmacHex(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

func equal(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

// CheckWebhook verifies a call to a webhook job that requires signatures,
// and drops repeats of a delivery already accepted (senders retry).
func (e *Engine) CheckWebhook(ctx context.Context, j store.Job, body []byte, h http.Header) error {
	if j.HookAuth == "" {
		return nil
	}
	secret, err := e.Secrets.Get(ctx, hookSecretName(j.ID))
	if err != nil || secret == "" {
		return ErrBadSignature // fail closed
	}
	ok := false
	switch j.HookAuth {
	case "github":
		got, found := strings.CutPrefix(h.Get("X-Hub-Signature-256"), "sha256=")
		ok = found && equal(strings.ToLower(got), hmacHex(secret, body))
	case "gitea":
		got := h.Get("X-Gitea-Signature")
		if got == "" {
			got = h.Get("X-Forgejo-Signature")
		}
		ok = got != "" && equal(strings.ToLower(got), hmacHex(secret, body))
	case "gitlab":
		got := h.Get("X-Gitlab-Token")
		ok = got != "" && equal(got, secret)
	}
	if !ok {
		e.WebhookRefused(ctx, j, ErrBadSignature)
		return ErrBadSignature
	}
	if key := deliveryKey(j, h); key != "" {
		if fresh, err := e.DB.ClaimNotice(ctx, key, 7*24*time.Hour); err == nil && !fresh {
			return ErrDuplicate
		}
	}
	return nil
}

// deliveryKey names the claim on one delivery to job j, or "" when the
// call carries no delivery id.
func deliveryKey(j store.Job, h http.Header) string {
	for _, k := range []string{"X-GitHub-Delivery", "X-Gitea-Delivery", "X-Forgejo-Delivery", "X-Gitlab-Event-UUID"} {
		if id := h.Get(k); id != "" {
			return fmt.Sprintf("hookdelivery:%d:%s", j.ID, truncate(id, 80))
		}
	}
	return ""
}

// ReleaseDelivery drops the claim CheckWebhook made, for a delivery that
// was then refused (paused, busy, rate-limited), so the sender's retry
// can still run.
func (e *Engine) ReleaseDelivery(ctx context.Context, j store.Job, h http.Header) {
	if key := deliveryKey(j, h); key != "" {
		_ = e.DB.ReleaseNotice(ctx, key)
	}
}

// WebhookEvent names the event a call is about, from the usual headers.
func WebhookEvent(h http.Header) string {
	for _, k := range []string{"X-GitHub-Event", "X-Gitea-Event", "X-Forgejo-Event", "X-Gitlab-Event"} {
		if v := h.Get(k); v != "" {
			return truncate(v, 80)
		}
	}
	return ""
}

// setHookSignature turns signature checking on (with a fresh secret, DMed
// to the job's owner) or off.
func (e *Engine) setHookSignature(ctx context.Context, t *turn, j *store.Job, style string) (string, error) {
	if j.Kind != store.JobWebhook {
		return "", fmt.Errorf("job #%d runs on a schedule, not a webhook", j.ID)
	}
	style = strings.ToLower(strings.TrimSpace(style))
	if style == "" {
		// Only an explicit "none" removes protection.
		return "", errors.New("say which signature style: github, gitea, gitlab, or none to turn signatures off")
	}
	if style == "none" {
		if err := e.DB.SetJobHookAuth(ctx, j.ID, ""); err != nil {
			return "", err
		}
		_ = e.Secrets.Delete(ctx, hookSecretName(j.ID))
		e.logActivity(ctx, "info", "job", j.Character, t.user.ID, fmt.Sprintf("webhook job #%d no longer requires signatures", j.ID), "")
		return fmt.Sprintf("Job #%d no longer checks signatures: the URL is its only secret again.", j.ID), nil
	}
	if _, ok := hookStyles[style]; !ok {
		return "", fmt.Errorf("signature style must be github, gitea (also Forgejo), gitlab or none")
	}
	if err := e.rotateHookSecret(ctx, t, j, style); err != nil {
		return "", err
	}
	if err := e.DB.SetJobHookAuth(ctx, j.ID, style); err != nil {
		return "", err
	}
	e.logActivity(ctx, "info", "job", j.Character, t.user.ID, fmt.Sprintf("webhook job #%d now requires %s signatures", j.ID, style), "")
	return fmt.Sprintf("Job #%d now only runs for calls signed the %s way. Thicket DMed the user the secret to paste into %s's webhook settings (you can't see it). Unsigned calls are refused.",
		j.ID, hookStyles[style], hookStyles[style]), nil
}

// rotateHookSecret stores a new signing secret and DMs it, with where it
// goes for that sender.
func (e *Engine) rotateHookSecret(ctx context.Context, t *turn, j *store.Job, style string) (err error) {
	secret, err := newHookSecret()
	if err != nil {
		return err
	}
	// The active secret is only replaced once the new one has reached the
	// user: until then, the one the sender already has keeps working.
	name := hookSecretName(j.ID)
	owner, ok := e.Chars.Get(j.Character)
	if !ok {
		owner = t.char
	}
	user := t.user
	if j.UserID != t.user.ID {
		if user, err = e.DB.UserByID(ctx, j.UserID); err != nil {
			return err
		}
	}
	where := map[string]string{
		"github": "In the repository's Settings → Webhooks, set Content type to application/json and paste it as the Secret.",
		"gitea":  "In the repository's Settings → Webhooks, paste it as the Secret.",
		"gitlab": "In the project's Settings → Webhooks, paste it as the Secret token.",
	}[style]
	msg := fmt.Sprintf("🔏 Signing secret for webhook “%s” (job #%d):\n`%s`\n%s Calls without a matching signature are refused from now on. Keep it private.", j.Name, j.ID, secret, where)
	e.Secrets.Remember(secret)
	if err := e.dmAs(ctx, user, owner, msg); err != nil {
		return err
	}
	if err := e.Secrets.Put(ctx, name, secret); err != nil {
		return fmt.Errorf("the new signing secret was sent but couldn't be saved, so it isn't in use: the previous signature setting still applies (try again): %w", err)
	}
	return nil
}

// dmAs sends a code-written DM that stays out of the conversation history.
func (e *Engine) dmAs(ctx context.Context, u *store.User, c *character.Character, msg string) error {
	ch := e.channelFor(u.Channel)
	if ch == nil {
		return fmt.Errorf("no channel %q for user %d", u.Channel, u.ID)
	}
	_, err := ch.SendDirect(ctx, u.ExternalID, e.persona(c), msg)
	return err
}
