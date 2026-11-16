package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/theshield2594/thicket/internal/channel"
	"github.com/theshield2594/thicket/internal/store"
)

// startSlow begins a reply that hangs, returning when the model is busy
// and a channel closed when the reply's handling ends.
func startSlow(t *testing.T, a *App, fm *fakeModel, ctx context.Context, in channel.Incoming) chan struct{} {
	t.Helper()
	fm.mu.Lock()
	fm.entered, fm.release = make(chan struct{}, 4), make(chan struct{})
	entered := fm.entered
	fm.mu.Unlock()
	done := make(chan struct{})
	go func() { a.Engine.HandleIncoming(ctx, in); close(done) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the slow reply never started")
	}
	return done
}

func wait(t *testing.T, done chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the reply was not stopped")
	}
}

func texts(out []sent) string {
	var b strings.Builder
	for _, s := range out {
		b.WriteString(s.persona + ": " + s.text + "\n")
	}
	return b.String()
}

func TestStopReplyInProgress(t *testing.T) {
	a, fc, fm, ctx := setup(t)
	onboard(t, a, fc, ctx)
	done := startSlow(t, a, fm, ctx, dm("please do the slow task"))

	// A message sent meanwhile queues behind the reply...
	queued := make(chan struct{})
	go func() { a.Engine.HandleIncoming(ctx, dm("and another thing")); close(queued) }()
	for i := 0; a.Engine.Queued("discord:dm-owner") == 0; i++ {
		if i > 500 {
			t.Fatal("second message never queued")
		}
		time.Sleep(2 * time.Millisecond)
	}
	calls := fm.calls

	// ...and "stop" doesn't: it cancels at once.
	a.Engine.HandleIncoming(ctx, dm("Stop!"))
	wait(t, done)
	wait(t, queued)
	out := texts(fc.take())
	if !strings.Contains(out, "Okay, stopped.") || !strings.Contains(out, "set aside the 1 message") {
		t.Fatalf("stop acknowledgement:\n%s", out)
	}
	if strings.Contains(out, "Finished the slow task") || strings.Contains(out, "went wrong") {
		t.Fatalf("a stopped reply still answered:\n%s", out)
	}
	if fm.calls != calls {
		t.Fatal("the queued message was answered after the stop")
	}

	// The next reply knows it was stopped, and life goes on.
	a.Engine.HandleIncoming(ctx, dm("hello again"))
	if out := texts(fc.take()); !strings.Contains(out, "Lovely to meet you") {
		t.Fatalf("after a stop: %s", out)
	}
	if got := allUserContent(lastChatReq(fm)); !strings.Contains(got, "stopped the reply before it was finished") {
		t.Fatal("history doesn't record the stop")
	}
}

func TestStopReportsWhatAlreadyHappened(t *testing.T) {
	a, fc, fm, ctx := setup(t)
	onboard(t, a, fc, ctx)
	done := startSlow(t, a, fm, ctx, dm("do the slow tool task"))
	a.Engine.HandleIncoming(ctx, dm("never mind"))
	wait(t, done)
	if out := texts(fc.take()); !strings.Contains(out, "Before you stopped me I had already done this: remember") {
		t.Fatalf("stop should say what already ran:\n%s", out)
	}
	owner, _ := a.DB.UserByExternal(ctx, "discord", "owner")
	if mems, _ := a.DB.AllMemories(ctx, owner.ID); len(mems) != 1 {
		t.Fatal("work done before the stop should stay done")
	}
}

func TestOnlyTheUserOrAnAdminCanStop(t *testing.T) {
	a, fc, fm, ctx := setup(t)
	onboard(t, a, fc, ctx)
	a.DB.CreateUser(ctx, "discord", "member", "Kim", "member")
	group := func(user, text string) channel.Incoming {
		return channel.Incoming{Channel: "discord", ChatID: "general", MessageID: user + text, UserID: user, Text: text, Listening: true}
	}
	done := startSlow(t, a, fm, ctx, group("owner", "the slow task, please"))
	go a.Engine.HandleIncoming(ctx, group("member", "stop"))
	time.Sleep(50 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("a member stopped the owner's reply")
	default:
	}
	close(fm.release)
	wait(t, done)
	if out := texts(fc.take()); !strings.Contains(out, "Finished the slow task") {
		t.Fatalf("the owner's reply should finish:\n%s", out)
	}
}

func TestStopRunningJob(t *testing.T) {
	a, fc, fm, ctx := setup(t)
	onboard(t, a, fc, ctx)
	owner, _ := a.DB.UserByExternal(ctx, "discord", "owner")
	j, _ := a.DB.CreateJob(ctx, store.Job{UserID: owner.ID, Character: "juniper", Name: "Big report", Kind: store.JobWebhook, HookHash: "h", Prompt: "Do the slow task."})
	fm.mu.Lock()
	fm.entered, fm.release = make(chan struct{}, 4), make(chan struct{})
	fm.mu.Unlock()
	if err := a.Scheduler.Fire(*j); err != nil {
		t.Fatal(err)
	}
	<-fm.entered
	a.Engine.HandleIncoming(ctx, dm("stop"))
	a.Scheduler.Wait()
	out := texts(fc.take())
	if !strings.Contains(out, "stopped “Big report”") || strings.Contains(out, "didn't work") {
		t.Fatalf("job stop:\n%s", out)
	}
	runs, _ := a.DB.JobRuns(ctx, j.ID, 1)
	if len(runs) != 1 || runs[0].Status != "error" || runs[0].Error != "stopped by the user" {
		t.Fatalf("run history: %+v", runs)
	}
}

func TestStopWithNothingRunningIsAMessage(t *testing.T) {
	a, fc, _, ctx := setup(t)
	onboard(t, a, fc, ctx)
	a.Engine.HandleIncoming(ctx, dm("stop"))
	if out := texts(fc.take()); !strings.Contains(out, "Lovely to meet you") {
		t.Fatalf("an idle stop should reach the sprig: %s", out)
	}
}

func TestStopOverAPI(t *testing.T) {
	a, _, fm, _, srv, token := setupAPI(t)
	fm.mu.Lock()
	fm.entered, fm.release = make(chan struct{}, 4), make(chan struct{})
	entered := fm.entered
	fm.mu.Unlock()
	res := make(chan apiResp, 1)
	go func() {
		res <- apiCall(t, srv, token, "POST", "/api/v1/messages", map[string]string{"text": "the slow task", "conversation": "work"})
	}()
	<-entered
	var stop struct {
		Reply bool `json:"stopped_reply"`
	}
	r := apiCallRaw(t, srv, token, "POST", "/api/v1/stop", map[string]string{"conversation": "work"}, &stop)
	if r != 200 || !stop.Reply {
		t.Fatalf("stop: %d %+v", r, stop)
	}
	got := <-res
	if len(got.Replies) != 1 || !strings.Contains(got.Replies[0].Text, "Okay, stopped.") {
		t.Fatalf("the stopped request's response: %+v", got)
	}
	_ = a
}
