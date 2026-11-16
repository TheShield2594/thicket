package app

// Regression tests for the review of the feature branch: each checks one
// finding's fix end to end.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/theshield2594/thicket/internal/channel"
	"github.com/theshield2594/thicket/internal/scheduler"
	"github.com/theshield2594/thicket/internal/store"
)

func TestStopIsNotRateLimited(t *testing.T) {
	a, fc, fm, ctx := setup(t)
	onboard(t, a, fc, ctx)
	for i := 0; i < 17; i++ { // 20 a minute: 2 to set up, 17, then the slow one
		a.Engine.HandleIncoming(ctx, dm(fmt.Sprintf("message %d", i)))
	}
	fc.take()
	done := startSlow(t, a, fm, ctx, dm("now the slow task"))
	a.Engine.HandleIncoming(ctx, dm("stop"))
	wait(t, done)
	if out := texts(fc.take()); !strings.Contains(out, "Okay, stopped.") {
		t.Fatalf("a stop over the rate limit was dropped:\n%s", out)
	}
}

func TestRefusedDeliveryCanBeRetried(t *testing.T) {
	a, fc, _, ctx := setup(t)
	onboard(t, a, fc, ctx)
	a.Engine.HandleIncoming(ctx, dm("make me a signed webhook for CI"))
	token, secret := takeHook(t, fc)
	body := []byte(`{"x":1}`)
	h := http.Header{"X-Hub-Signature-256": {"sha256=" + sign(secret, body)}, "X-Github-Delivery": {"d-9"}}
	a.Engine.HandleIncoming(ctx, dm("pause job 1"))
	fc.take()
	if code, _ := a.Webhook(ctx, token, body, h); code != http.StatusConflict {
		t.Fatalf("paused: %d", code)
	}
	a.DB.SetJobEnabled(ctx, 1, true)
	if code, msg := a.Webhook(ctx, token, body, h); code != http.StatusAccepted {
		t.Fatalf("the sender's retry of a refused delivery: %d %s", code, msg)
	}
	a.Scheduler.Wait()
}

func TestSignatureSafety(t *testing.T) {
	a, fc, fm, ctx := setup(t)
	onboard(t, a, fc, ctx)
	a.Engine.HandleIncoming(ctx, dm("make me a signed webhook for CI"))
	token, secret := takeHook(t, fc)
	body := []byte(`{"x":1}`)
	signed := func(s string) http.Header { return http.Header{"X-Hub-Signature-256": {"sha256=" + sign(s, body)}} }

	// set_signature with no style is refused, not taken as "none".
	a.Engine.HandleIncoming(ctx, dm("sign job 1 with"))
	fc.take()
	if r := lastToolResult(fm); !strings.Contains(r, "say which signature style") {
		t.Fatalf("empty signature: %s", r)
	}
	if code, _ := a.Webhook(ctx, token, body, nil); code != http.StatusUnauthorized {
		t.Fatalf("signatures were turned off by an empty value: %d", code)
	}

	// A new secret that can't be delivered leaves the old one working.
	fc.mu.Lock()
	fc.failIf = func(s string) bool { return strings.Contains(s, "Signing secret") }
	fc.mu.Unlock()
	a.Engine.HandleIncoming(ctx, dm("sign job 1 with github"))
	fc.mu.Lock()
	fc.failIf = nil
	fc.mu.Unlock()
	fc.take()
	if code, _ := a.Webhook(ctx, token, body, signed(secret)); code != http.StatusAccepted {
		t.Fatalf("old secret after a failed rotation: %d", code)
	}
	a.Scheduler.Wait()
	fc.take()

	// A new secret that's delivered but can't be saved isn't put in use.
	sqlDB := a.DB.DB()
	for _, op := range []string{"INSERT", "UPDATE"} {
		if _, err := sqlDB.Exec("CREATE TRIGGER no_secret_" + op + " BEFORE " + op + " ON secrets BEGIN SELECT RAISE(ABORT, 'disk full'); END"); err != nil {
			t.Fatal(err)
		}
	}
	a.Engine.HandleIncoming(ctx, dm("sign job 1 with github"))
	for _, op := range []string{"INSERT", "UPDATE"} {
		sqlDB.Exec("DROP TRIGGER no_secret_" + op)
	}
	fc.take()
	if r := lastToolResult(fm); !strings.Contains(r, "isn't in use") {
		t.Fatalf("a secret that couldn't be saved: %s", r)
	}
	readyAgain(a)
	if code, _ := a.Webhook(ctx, token, body, signed(secret)); code != http.StatusAccepted {
		t.Fatalf("old secret after a rotation that couldn't be saved: %d", code)
	}
	a.Scheduler.Wait()
	fc.take()

	// A stored secret that can't be read isn't touched by a failed rotation.
	if _, err := sqlDB.Exec("UPDATE secrets SET ciphertext = x'00' WHERE name = 'webhook_1_signing'"); err != nil {
		t.Fatal(err)
	}
	var before []byte
	sqlDB.QueryRow("SELECT ciphertext FROM secrets WHERE name = 'webhook_1_signing'").Scan(&before)
	fc.mu.Lock()
	fc.failIf = func(s string) bool { return strings.Contains(s, "Signing secret") }
	fc.mu.Unlock()
	a.Engine.HandleIncoming(ctx, dm("sign job 1 with github"))
	fc.mu.Lock()
	fc.failIf = nil
	fc.mu.Unlock()
	fc.take()
	var after []byte
	sqlDB.QueryRow("SELECT ciphertext FROM secrets WHERE name = 'webhook_1_signing'").Scan(&after)
	if string(before) != string(after) || len(after) == 0 {
		t.Fatalf("a failed rotation changed an unreadable secret: %x -> %x", before, after)
	}

	// A webhook whose signature setup fails isn't left accepting unsigned
	// calls, including while the setup is still going on.
	duringSetup := 0
	fc.mu.Lock()
	fc.failIf = func(s string) bool {
		if !strings.Contains(s, "Signing secret") {
			return false
		}
		for _, m := range fc.sent { // fc.mu is held here
			if tok := tokenRe.FindString(m.text); tok != "" && strings.Contains(m.text, "/hooks/") {
				duringSetup, _ = a.Webhook(ctx, tok, body, nil)
			}
		}
		return true
	}
	fc.mu.Unlock()
	a.Engine.HandleIncoming(ctx, dm("another signed webhook for CI"))
	fc.mu.Lock()
	fc.failIf = nil
	fc.mu.Unlock()
	t2, _ := takeHook(t, fc)
	if t2 == "" {
		t.Fatal("the URL should have been sent before signatures failed")
	}
	if duringSetup != http.StatusUnauthorized {
		t.Fatalf("an unsigned call while signatures were being set up: %d", duringSetup)
	}
	if code, _ := a.Webhook(ctx, t2, body, nil); code != http.StatusNotFound {
		t.Fatalf("half-set-up signed webhook still runs: %d", code)
	}

	// If it can't be removed either, it's paused and still refuses calls.
	if _, err := sqlDB.Exec("CREATE TRIGGER no_job_delete BEFORE DELETE ON jobs BEGIN SELECT RAISE(ABORT, 'locked'); END"); err != nil {
		t.Fatal(err)
	}
	fc.mu.Lock()
	fc.failIf = func(s string) bool { return strings.Contains(s, "Signing secret") }
	fc.mu.Unlock()
	a.Engine.HandleIncoming(ctx, dm("a third signed webhook for CI"))
	fc.mu.Lock()
	fc.failIf = nil
	fc.mu.Unlock()
	sqlDB.Exec("DROP TRIGGER no_job_delete")
	t3, _ := takeHook(t, fc)
	if r := lastToolResult(fm); !strings.Contains(r, "it's paused") {
		t.Fatalf("a webhook that couldn't be removed: %s", r)
	}
	if code, _ := a.Webhook(ctx, t3, body, nil); code == http.StatusAccepted {
		t.Fatal("a signed webhook left behind accepted an unsigned call")
	}
	jobs, _ := a.DB.EnabledJobs(ctx)
	for _, j := range jobs {
		if j.HookAuth != "" && j.ID > 1 {
			t.Fatalf("job #%d was left enabled", j.ID)
		}
	}
}

func TestFireRereadsTheLastRun(t *testing.T) {
	a, fc, _, ctx := setup(t)
	onboard(t, a, fc, ctx)
	owner, _ := a.DB.UserByExternal(ctx, "discord", "owner")
	j, _ := a.DB.CreateJob(ctx, store.Job{UserID: owner.ID, Character: "juniper", Name: "x", Kind: store.JobWebhook, HookHash: "h", Prompt: "x"})
	stale := *j // read before the run below
	if err := a.Scheduler.Fire(*j); err != nil {
		t.Fatal(err)
	}
	a.Scheduler.Wait()
	if err := a.Scheduler.Fire(stale); err != scheduler.ErrTooSoon {
		t.Fatalf("a stale copy of the job slipped past the gap: %v", err)
	}
}

func TestFirstOpenRouterKeyKeepsChosenModels(t *testing.T) {
	a, fc, _, ctx := setup(t)
	a.Settings.Set(ctx, "model_url", "https://openrouter.ai/api/v1")
	a.Settings.Set(ctx, "model", "anthropic/claude-opus-5.5")
	a.Engine.HandleIncoming(ctx, dm("hi"))
	a.Engine.HandleIncoming(ctx, dm("sk-or-v1-"+strings.Repeat("ab12", 16)))
	fc.take()
	if got := a.Settings.String(ctx, "model"); got != "anthropic/claude-opus-5.5" {
		t.Fatalf("the chosen OpenRouter model was reset: %q", got)
	}
}

func TestReplacementServerAddressWins(t *testing.T) {
	os.Setenv("THICKET_MODEL_URL", "http://127.0.0.1:1/v1") // configured, but down
	t.Cleanup(func() { os.Unsetenv("THICKET_MODEL_URL") })
	a, fc, _, ctx := setup(t)
	srv := fakeOllama(t, `{"id":"qwen2.5:14b"}`)
	a.Engine.HandleIncoming(ctx, dm(srv.URL))
	if out := texts(fc.take()); !strings.Contains(out, "Connected to "+srv.URL) {
		t.Fatalf("the address sent in chat was ignored:\n%s", out)
	}
}

func TestNoticeDoesNotSwitchSprig(t *testing.T) {
	a, fc, fm, ctx := setup(t)
	onboard(t, a, fc, ctx)
	withPip(t, ctx, a)
	a.Engine.HandleIncoming(ctx, dm("Pip, hello"))
	a.Engine.Notify(ctx, "test-notice", time.Hour, "warn", "a notice")
	a.Engine.HandleIncoming(ctx, dm("and tomorrow?"))
	fc.take()
	if p := systemPromptOf(lastChatReq(fm)); !strings.HasPrefix(p, "You are Pip") {
		t.Fatal("a notice from Juni took the conversation away from Pip")
	}
}

func TestOverflowRetryKeepsTheImage(t *testing.T) {
	a, fc, fm, ctx := setup(t)
	onboard(t, a, fc, ctx)
	a.Engine.FetchAttachment = fakeFetch
	chatter(t, ctx, a, fc, 1, 6)
	fm.mu.Lock()
	fm.contextLimit = 1800
	fm.compactFail = true
	fm.mu.Unlock()
	in := dm("what's in this picture?")
	in.Attachments = []channel.Attachment{{Name: "cat.png", URL: "https://cdn.example/cat.png", ContentType: "image/png", Size: 8}}
	a.Engine.HandleIncoming(ctx, in) // the overflow notice lands in this same conversation
	fc.take()
	req := lastChatReq(fm)
	var withImage bool
	for _, m := range req.Messages {
		withImage = withImage || (len(m.Images) == 1 && strings.Contains(m.Content, "what's in this picture?"))
	}
	if !withImage {
		t.Fatal("the retry lost the image (or put it on the wrong message)")
	}
}

func TestCompactionFoldsFromTheOldest(t *testing.T) {
	a, fc, fm, ctx := setup(t)
	onboard(t, a, fc, ctx)
	owner, _ := a.DB.UserByExternal(ctx, "discord", "owner")
	conv, _ := a.DB.OpenConversation(ctx, owner.ID, "discord:dm-owner", a.Cfg.IdleAfter)
	for i := 0; i < 450; i++ { // more than one read's worth
		a.DB.AddMessage(ctx, conv.ID, "user", "", fmt.Sprintf("old-%03d %s", i, strings.Repeat("x", 80)))
	}
	a.Settings.Set(ctx, "history_tokens", "600")
	a.Engine.HandleIncoming(ctx, dm("hello"))
	fc.take()
	var fold string
	fm.mu.Lock()
	for _, r := range fm.reqs {
		if strings.HasPrefix(r.Messages[0].Content, "You keep the running summary") && fold == "" {
			fold = r.Messages[1].Content
		}
	}
	fm.mu.Unlock()
	if !strings.Contains(fold, "old-000") {
		t.Fatal("the first summary must start from the oldest unsummarised message")
	}
}

func TestAPIStopSetsAsideWaitingRequests(t *testing.T) {
	a, _, fm, _, srv, token := setupAPI(t)
	fm.mu.Lock()
	fm.entered, fm.release = make(chan struct{}, 4), make(chan struct{})
	entered := fm.entered
	fm.mu.Unlock()
	first := make(chan apiResp, 1)
	go func() {
		first <- apiCall(t, srv, token, "POST", "/api/v1/messages", map[string]string{"text": "the slow task", "conversation": "q"})
	}()
	<-entered
	second := make(chan apiResp, 1)
	go func() {
		second <- apiCall(t, srv, token, "POST", "/api/v1/messages", map[string]string{"text": "and then this", "conversation": "q"})
	}()
	for i := 0; a.API.Queued("t1.q") == 0; i++ {
		if i > 500 {
			t.Fatal("second request never queued")
		}
		time.Sleep(2 * time.Millisecond)
	}
	var stop struct {
		SetAside int `json:"set_aside"`
	}
	apiCallRaw(t, srv, token, "POST", "/api/v1/stop", map[string]string{"conversation": "q"}, &stop)
	<-first
	if r := <-second; r.Code != http.StatusConflict || stop.SetAside != 1 {
		t.Fatalf("waiting request after a stop: %d (set aside %d)", r.Code, stop.SetAside)
	}
	// A malformed stop body is refused rather than stopping "default".
	if code := apiCallRaw(t, srv, token, "POST", "/api/v1/stop", map[string]any{"conversation": 123}, &stop); code != http.StatusBadRequest {
		t.Fatalf("malformed stop body: %d", code)
	}
	_ = context.Background
}
