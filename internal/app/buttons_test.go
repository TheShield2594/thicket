package app

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/theshield2594/thicket/internal/channel"
	"github.com/theshield2594/thicket/internal/store"
)

// buttonChannel is a fake chat app with message buttons.
type buttonChannel struct {
	*fakeChannel
	bmu     sync.Mutex
	actions [][]channel.Action // per SendActions call
	press   channel.PressHandler
}

func (b *buttonChannel) SendActions(ctx context.Context, chatID string, p channel.Persona, text string, acts []channel.Action) ([]string, error) {
	ids, err := b.SendIDs(ctx, chatID, p, text)
	if err != nil {
		return nil, err
	}
	b.bmu.Lock()
	b.actions = append(b.actions, acts)
	b.bmu.Unlock()
	return ids, nil
}

func (b *buttonChannel) OnPress(h channel.PressHandler) { b.press = h }

func (b *buttonChannel) lastActions() []channel.Action {
	b.bmu.Lock()
	defer b.bmu.Unlock()
	if len(b.actions) == 0 {
		return nil
	}
	return b.actions[len(b.actions)-1]
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for " + what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestApprovalButtons(t *testing.T) {
	bc := &buttonChannel{fakeChannel: &fakeChannel{}}
	a, _, ctx := setupWith(t, bc)
	e := a.Engine
	if bc.press == nil {
		t.Fatal("the engine didn't register for button presses")
	}
	e.HandleIncoming(ctx, dm("hi"))
	e.HandleIncoming(ctx, dm("sk-or-v1-"+strings.Repeat("cd34", 16)))
	bc.take()
	owner, _ := a.DB.UserByExternal(ctx, "discord", "owner")
	if _, err := a.DB.CreateUser(ctx, "discord", "kim", "", "member"); err != nil {
		t.Fatal(err)
	}
	a.DB.SetToolLevel(ctx, store.AllSprigs, "remember", store.LevelAsk)

	e.HandleIncoming(ctx, dm("remember my dog is called Biscuit"))
	out := bc.take()
	if len(out) != 2 || !strings.Contains(out[1].text, "Reply **approve** or **deny**") {
		t.Fatalf("approval prompt: %+v", out)
	}
	acts := bc.lastActions()
	if len(acts) != 2 || acts[0].Label != "Approve" || acts[1].Label != "Deny" || !strings.HasSuffix(acts[0].ID, ":approve") {
		t.Fatalf("buttons: %+v", acts)
	}
	press := func(user, chat, action string) channel.PressReply {
		return bc.press(ctx, channel.Press{Channel: "discord", ChatID: chat, MessageID: "out-2", UserID: user, UserName: "x", IsDM: true, ActionID: action})
	}

	// Strangers, other members, and the right person in another chat are
	// all refused privately, and nothing changes.
	for _, tc := range []struct{ user, chat string }{{"stranger", "dm-owner"}, {"kim", "dm-owner"}, {"owner", "elsewhere"}} {
		r := press(tc.user, tc.chat, acts[0].ID)
		if r.Notice != "This isn't yours to approve." || r.Outcome != "" {
			t.Fatalf("%s in %s: %+v", tc.user, tc.chat, r)
		}
	}
	if aps, _ := a.DB.PendingApprovals(ctx, owner.ID, time.Hour); len(aps) != 1 {
		t.Fatalf("approval should still be pending: %+v", aps)
	}

	// The owner taps Approve: the message settles at once, then the
	// action runs and the sprig reports back.
	if r := press("owner", "dm-owner", acts[0].ID); r.Outcome != "✅ Approved" || r.Notice != "" {
		t.Fatalf("approve: %+v", r)
	}
	eventually(t, "the memory", func() bool {
		mems, _ := a.DB.AllMemories(ctx, owner.ID)
		return len(mems) == 1
	})
	eventually(t, "the sprig's report", func() bool { return len(bc.peek()) == 1 })
	bc.take()
	runs, _ := a.DB.RecentActivity(ctx, 50, "info")
	if !hasActivity(runs, "with a button") || !hasActivity(runs, "ran approved tool remember") {
		t.Fatal("button approval not logged")
	}

	// A second tap (or a stale button) changes nothing.
	if r := press("owner", "dm-owner", acts[0].ID); r.Outcome != "✅ Approved" || !strings.Contains(r.Notice, "already answered") {
		t.Fatalf("second tap: %+v", r)
	}
	if r := press("owner", "dm-owner", "approval:999:approve"); r.Outcome != "" || r.Notice == "" {
		t.Fatalf("unknown approval: %+v", r)
	}

	// Deny: nothing runs.
	e.HandleIncoming(ctx, dm("remember my dog likes walks"))
	bc.take()
	acts = bc.lastActions()
	if r := press("owner", "dm-owner", acts[1].ID); r.Outcome != "✖️ Denied" {
		t.Fatalf("deny: %+v", r)
	}
	eventually(t, "the sprig's acknowledgement", func() bool { return len(bc.peek()) == 1 })
	if mems, _ := a.DB.AllMemories(ctx, owner.ID); len(mems) != 1 {
		t.Fatalf("denied action ran: %+v", mems)
	}
}

// Typing still answers an approval shown with buttons.
func TestApprovalButtonsStillAcceptTyping(t *testing.T) {
	bc := &buttonChannel{fakeChannel: &fakeChannel{}}
	a, _, ctx := setupWith(t, bc)
	e := a.Engine
	e.HandleIncoming(ctx, dm("hi"))
	e.HandleIncoming(ctx, dm("sk-or-v1-"+strings.Repeat("cd34", 16)))
	owner, _ := a.DB.UserByExternal(ctx, "discord", "owner")
	a.DB.SetToolLevel(ctx, store.AllSprigs, "remember", store.LevelAsk)
	e.HandleIncoming(ctx, dm("remember my dog is called Biscuit"))
	acts := bc.lastActions()
	e.HandleIncoming(ctx, dm("approve"))
	if mems, _ := a.DB.AllMemories(ctx, owner.ID); len(mems) != 1 {
		t.Fatalf("typed approval didn't run: %+v", mems)
	}
	if r := bc.press(ctx, channel.Press{Channel: "discord", ChatID: "dm-owner", UserID: "owner", ActionID: acts[0].ID}); r.Outcome != "✅ Approved" {
		t.Fatalf("stale button: %+v", r)
	}
}

// A prompt that never arrived leaves nothing to answer, by text or button.
func TestUndeliveredButtonApproval(t *testing.T) {
	bc := &buttonChannel{fakeChannel: &fakeChannel{}}
	a, _, ctx := setupWith(t, bc)
	e := a.Engine
	e.HandleIncoming(ctx, dm("hi"))
	e.HandleIncoming(ctx, dm("sk-or-v1-"+strings.Repeat("cd34", 16)))
	owner, _ := a.DB.UserByExternal(ctx, "discord", "owner")
	a.DB.SetToolLevel(ctx, store.AllSprigs, "remember", store.LevelAsk)
	bc.mu.Lock()
	bc.failIf = func(s string) bool { return strings.Contains(s, "I need your OK") }
	bc.mu.Unlock()
	e.HandleIncoming(ctx, dm("remember my dog is called Biscuit"))
	if aps, _ := a.DB.PendingApprovals(ctx, owner.ID, time.Hour); len(aps) != 0 {
		t.Fatalf("undelivered approval is pending: %+v", aps)
	}
	if r := bc.press(ctx, channel.Press{Channel: "discord", ChatID: "dm-owner", UserID: "owner", ActionID: "approval:1:approve"}); r.Outcome != "" {
		t.Fatalf("undelivered approval answered: %+v", r)
	}
}
