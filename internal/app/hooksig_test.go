package app

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"
)

var secretRe = regexp.MustCompile(`thk_whs_[A-Za-z0-9_-]{30,}`)

func sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

// takeHook reads the URL token and signing secret from Thicket's DMs.
func takeHook(t *testing.T, fc *fakeChannel) (token, secret string) {
	t.Helper()
	for _, s := range fc.take() {
		if m := tokenRe.FindString(s.text); m != "" && strings.Contains(s.text, "/hooks/") {
			token = m
		}
		if m := secretRe.FindString(s.text); m != "" {
			secret = m
		}
	}
	return token, secret
}

// readyAgain skips the one-minute gap between webhook runs by moving the
// scheduler's clock on.
func readyAgain(a *App) {
	skip := 2 * time.Minute
	if prev := a.Scheduler.Now; prev != nil {
		a.Scheduler.Now = func() time.Time { return prev().Add(skip) }
	}
}

func TestSignedWebhooks(t *testing.T) {
	a, fc, fm, ctx := setup(t)
	onboard(t, a, fc, ctx)
	a.Engine.HandleIncoming(ctx, dm("make me a signed webhook for CI"))
	token, secret := takeHook(t, fc)
	if token == "" || secret == "" {
		t.Fatalf("want a URL and a signing secret DMed: %q %q", token, secret)
	}
	assertTokenHidden(t, a, fm, secret)
	var n int
	a.DB.DB().QueryRow("SELECT COUNT(*) FROM secrets WHERE CAST(ciphertext AS TEXT) LIKE ?", "%"+secret+"%").Scan(&n)
	if n != 0 {
		t.Fatal("signing secret stored unencrypted")
	}
	body := []byte(`{"action":"completed","conclusion":"failure"}`)
	hdr := func(sig, delivery string) http.Header {
		h := http.Header{"Content-Type": {"application/json"}, "X-Github-Event": {"workflow_run"}}
		if sig != "" {
			h.Set("X-Hub-Signature-256", sig)
		}
		if delivery != "" {
			h.Set("X-Github-Delivery", delivery)
		}
		return h
	}

	// Unsigned or wrongly signed: refused, and the owner hears once.
	if code, _ := a.Webhook(ctx, token, body, hdr("", "")); code != http.StatusUnauthorized {
		t.Fatalf("unsigned call: %d", code)
	}
	if code, _ := a.Webhook(ctx, token, body, hdr("sha256="+sign("wrong", body), "")); code != http.StatusUnauthorized {
		t.Fatalf("wrong signature: %d", code)
	}
	if code, _ := a.Webhook(ctx, token, []byte(`{"tampered":true}`), hdr("sha256="+sign(secret, body), "")); code != http.StatusUnauthorized {
		t.Fatalf("signature for another body: %d", code)
	}
	if out := texts(fc.take()); strings.Count(out, "without a valid signature") != 1 {
		t.Fatalf("want one notice about bad signatures:\n%s", out)
	}

	// Signed: runs, and the sprig is told which event it was.
	if code, msg := a.Webhook(ctx, token, body, hdr("sha256="+sign(secret, body), "d-1")); code != http.StatusAccepted {
		t.Fatalf("signed call: %d %s", code, msg)
	}
	a.Scheduler.Wait()
	fc.take()
	var prompt string
	fm.mu.Lock()
	for _, r := range fm.reqs {
		for _, m := range r.Messages {
			if strings.Contains(m.Content, "[Webhook job") {
				prompt = m.Content
			}
		}
	}
	fm.mu.Unlock()
	if !strings.Contains(prompt, "Event: workflow_run") || !strings.Contains(prompt, `"conclusion":"failure"`) {
		t.Fatalf("job prompt: %s", prompt)
	}
	// The sender retrying the same delivery doesn't run it twice.
	readyAgain(a)
	if code, msg := a.Webhook(ctx, token, body, hdr("sha256="+sign(secret, body), "d-1")); code != http.StatusOK || msg != "already received" {
		t.Fatalf("repeated delivery: %d %s", code, msg)
	}

	// A new URL brings a new secret; the old one stops working.
	a.Engine.HandleIncoming(ctx, dm("new url for job 1"))
	token2, secret2 := takeHook(t, fc)
	if token2 == "" || secret2 == "" || secret2 == secret {
		t.Fatal("new_url should rotate the signing secret too")
	}
	readyAgain(a)
	if code, _ := a.Webhook(ctx, token2, body, hdr("sha256="+sign(secret, body), "d-2")); code != http.StatusUnauthorized {
		t.Fatalf("old secret: %d", code)
	}
	if code, _ := a.Webhook(ctx, token2, body, hdr("sha256="+sign(secret2, body), "d-3")); code != http.StatusAccepted {
		t.Fatalf("new secret: %d", code)
	}
	a.Scheduler.Wait()
	fc.take()

	// GitLab sends the secret itself; Forgejo signs like Gitea.
	a.Engine.HandleIncoming(ctx, dm("sign job 1 with gitlab"))
	_, glSecret := takeHook(t, fc)
	readyAgain(a)
	if code, _ := a.Webhook(ctx, token2, body, http.Header{"X-Gitlab-Token": {"nope"}}); code != http.StatusUnauthorized {
		t.Fatalf("gitlab wrong token: %d", code)
	}
	if code, _ := a.Webhook(ctx, token2, body, http.Header{"X-Gitlab-Token": {glSecret}, "X-Gitlab-Event": {"Pipeline Hook"}}); code != http.StatusAccepted {
		t.Fatalf("gitlab: %d", code)
	}
	a.Scheduler.Wait()
	fc.take()
	a.Engine.HandleIncoming(ctx, dm("sign job 1 with gitea"))
	_, gtSecret := takeHook(t, fc)
	readyAgain(a)
	if code, _ := a.Webhook(ctx, token2, body, http.Header{"X-Forgejo-Signature": {sign(gtSecret, body)}}); code != http.StatusAccepted {
		t.Fatalf("forgejo: %d", code)
	}
	a.Scheduler.Wait()
	fc.take()

	// Turned off, the URL alone is enough again, and the secret is gone.
	a.Engine.HandleIncoming(ctx, dm("sign job 1 with none"))
	fc.take()
	readyAgain(a)
	if code, _ := a.Webhook(ctx, token2, body, nil); code != http.StatusAccepted {
		t.Fatalf("signatures off: %d", code)
	}
	a.Scheduler.Wait()
	if a.Secrets.Has(ctx, "webhook_1_signing") {
		t.Fatal("signing secret kept after turning signatures off")
	}
}
