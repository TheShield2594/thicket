package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func withPip(t *testing.T, ctx context.Context, a *App) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(a.Cfg.CharactersDir, "pip.md"), []byte(pip), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.Chars.Load(); err != nil {
		t.Fatal(err)
	}
}

func TestAskSprig(t *testing.T) {
	a, fc, fm, ctx := setup(t)
	onboard(t, a, fc, ctx)
	withPip(t, ctx, a)

	a.Engine.HandleIncoming(ctx, dm("can you ask Pip what's on tomorrow?"))
	out := fc.take()
	// Only Juni speaks to the user; Pip answered Juni behind the scenes.
	if len(out) != 1 || out[0].persona != "Juni" {
		t.Fatalf("want one reply from Juni: %s", texts(out))
	}
	if r := lastToolResult(fm); !strings.Contains(r, "Pip answered") || !strings.Contains(r, `<untrusted source="sprig pip">`) || !strings.Contains(r, "dentist at 10") {
		t.Fatalf("Juni should get Pip's answer, as data: %s", r)
	}
	// Pip saw the question, not the conversation, and couldn't pull in
	// anyone else or reshape the conversation.
	var pipReq *struct{ tools []string }
	fm.mu.Lock()
	for _, r := range fm.reqs {
		if strings.HasPrefix(r.Messages[0].Content, "You are Pip") {
			last := r.Messages[len(r.Messages)-1].Content
			if !strings.Contains(last, "Juni") || !strings.Contains(last, "What's on the calendar tomorrow?") {
				t.Errorf("Pip's question: %s", last)
			}
			for _, m := range r.Messages {
				if strings.Contains(m.Content, "can you ask Pip") {
					t.Error("Pip saw the user's conversation")
				}
			}
			pipReq = &struct{ tools []string }{}
			for _, d := range r.Tools {
				pipReq.tools = append(pipReq.tools, d.Function.Name)
			}
		}
	}
	fm.mu.Unlock()
	if pipReq == nil {
		t.Fatal("Pip was never asked")
	}
	for _, name := range pipReq.tools {
		if name == "ask_sprig" || name == "handoff" || name == "delegate_task" || name == "schedule_job" {
			t.Errorf("an asked sprig was offered %s", name)
		}
	}
	// The exchange is part of the conversation from now on.
	a.Engine.HandleIncoming(ctx, dm("thanks"))
	fc.take()
	if got := allUserContent(lastChatReq(fm)); !strings.Contains(got, "Juni asked Pip: What's on the calendar tomorrow?") {
		t.Fatal("the exchange wasn't kept in the conversation")
	}
}

func TestAskedSprigCantAskOthers(t *testing.T) {
	a, fc, fm, ctx := setup(t)
	onboard(t, a, fc, ctx)
	withPip(t, ctx, a)
	a.Engine.HandleIncoming(ctx, dm("ask pip in a chain"))
	fc.take()
	fm.mu.Lock()
	defer fm.mu.Unlock()
	for _, r := range fm.reqs {
		for _, m := range r.Messages {
			if m.Role == "tool" && strings.Contains(m.Content, "can't ask others") {
				return // refused in code, even though the tool wasn't offered
			}
			if m.Role == "tool" && strings.Contains(m.Content, "don't have access") {
				return
			}
		}
	}
	t.Fatal("a chain of asks wasn't refused")
}

func TestDelegateTask(t *testing.T) {
	a, fc, fm, ctx := setup(t)
	onboard(t, a, fc, ctx)
	withPip(t, ctx, a)

	a.Engine.HandleIncoming(ctx, dm("have pip look into flight prices for May"))
	a.Engine.WaitTasks()
	out := fc.take()
	var juni, pipDM bool
	for _, s := range out {
		juni = juni || s.persona == "Juni"
		pipDM = pipDM || (s.persona == "Pip" && s.chat == "dm-owner")
	}
	if !juni || !pipDM {
		t.Fatalf("want Juni's reply and Pip's result by DM:\n%s", texts(out))
	}
	fm.mu.Lock()
	var saw bool
	for _, r := range fm.reqs {
		for _, m := range r.Messages {
			saw = saw || strings.Contains(m.Content, "[Background task from Juni]") && strings.Contains(m.Content, "flight prices for may")
		}
	}
	fm.mu.Unlock()
	if !saw {
		t.Fatal("Pip never got the task")
	}
}

func TestStopDelegatedTask(t *testing.T) {
	a, fc, fm, ctx := setup(t)
	onboard(t, a, fc, ctx)
	withPip(t, ctx, a)
	fm.mu.Lock()
	fm.entered, fm.release = make(chan struct{}, 4), make(chan struct{})
	fm.mu.Unlock()
	a.Engine.HandleIncoming(ctx, dm("have pip look into the slow task"))
	select {
	case <-fm.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("task never started")
	}
	fc.take()
	a.Engine.HandleIncoming(ctx, dm("stop"))
	a.Engine.WaitTasks()
	out := texts(fc.take())
	if !strings.Contains(out, "stopped “Pip's task") || strings.Contains(out, "couldn't finish") {
		t.Fatalf("stopping a background task:\n%s", out)
	}
}
