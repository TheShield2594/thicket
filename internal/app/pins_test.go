package app

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/theshield2594/thicket/internal/channel"
	"github.com/theshield2594/thicket/internal/store"
)

func dmID(id, text string) channel.Incoming {
	in := dm(text)
	in.MessageID = id
	return in
}

func replyTo(id, text string) channel.Incoming {
	in := dm(text)
	in.ReplyTo = id
	return in
}

// unhurried moves the engine's clock on with every look, so long scripted
// chats stay under the per-minute message limit.
func unhurried(a *App) {
	var mu sync.Mutex
	at := time.Now()
	a.Engine.Now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		at = at.Add(4 * time.Second)
		return at
	}
}

func convMessages(t *testing.T, ctx context.Context, a *App) []store.Message {
	t.Helper()
	owner, _ := a.DB.UserByExternal(ctx, "discord", "owner")
	conv, err := a.DB.CurrentConversation(ctx, owner.ID, "discord:dm-owner", a.Cfg.IdleAfter)
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := a.DB.RecentMessages(ctx, conv.ID, 500)
	if err != nil {
		t.Fatal(err)
	}
	return msgs
}

func hasContent(msgs []store.Message, s string) bool {
	for _, m := range msgs {
		if strings.Contains(m.Content, s) {
			return true
		}
	}
	return false
}

func TestPinnedMessageSurvivesCompaction(t *testing.T) {
	a, fc, fm, ctx := setup(t)
	onboard(t, a, fc, ctx)
	unhurried(a)
	a.Settings.Set(ctx, "history_tokens", "600") // 2,400 characters

	a.Engine.HandleIncoming(ctx, dmID("m-flight", "My flight is AA123 on Friday."))
	fc.take()
	a.Engine.HandleIncoming(ctx, replyTo("m-flight", "pin this"))
	out := fc.take()
	if len(out) != 1 || !strings.Contains(out[0].text, "Pinned") || !strings.Contains(out[0].text, "AA123") {
		t.Fatalf("pin acknowledgement: %+v", out)
	}
	if hasContent(convMessages(t, ctx, a), "pin this") {
		t.Fatal("the pin command was stored in the conversation")
	}
	a.Engine.HandleIncoming(ctx, replyTo("m-flight", "pin this"))
	if out := fc.take(); len(out) != 1 || !strings.Contains(out[0].text, "already pinned") {
		t.Fatalf("pinning twice: %+v", out)
	}

	chatter(t, ctx, a, fc, 1, 12)
	if fm.compactions == 0 {
		t.Fatal("the conversation was never summarised")
	}
	a.Engine.HandleIncoming(ctx, dm("when do I fly?"))
	fc.take()
	got := allUserContent(lastChatReq(fm))
	if !strings.Contains(got, "pinned these messages") || !strings.Contains(got, "AA123") {
		t.Fatalf("the pinned message dropped out of the prompt:\n%s", got)
	}

	a.Engine.HandleIncoming(ctx, dm("what's pinned?"))
	if out := fc.take(); len(out) != 1 || !strings.Contains(out[0].text, "AA123") {
		t.Fatalf("listing pins: %+v", out)
	}
	a.Engine.HandleIncoming(ctx, dm("unpin all"))
	if out := fc.take(); len(out) != 1 || !strings.Contains(out[0].text, "Unpinned 1 message") {
		t.Fatalf("unpin all: %+v", out)
	}
	a.Engine.HandleIncoming(ctx, dm("and now?"))
	fc.take()
	if got := allUserContent(lastChatReq(fm)); strings.Contains(got, "AA123") {
		t.Fatalf("an unpinned, summarised message is still in the prompt:\n%s", got)
	}
}

func TestPinThatPinsTheLatestMessage(t *testing.T) {
	a, fc, _, ctx := setup(t)
	onboard(t, a, fc, ctx)
	a.Engine.HandleIncoming(ctx, dm("Locker code is 4471."))
	fc.take()
	a.Engine.HandleIncoming(ctx, dm("Pin that, please."))
	out := fc.take()
	if len(out) != 1 || !strings.Contains(out[0].text, "Pinned") {
		t.Fatalf("pin that: %+v", out)
	}
	pinned := 0
	for _, m := range convMessages(t, ctx, a) {
		if m.Pinned {
			pinned++
			if m.Role != "assistant" {
				t.Fatalf("pinned the wrong message: %+v", m)
			}
		}
	}
	if pinned != 1 {
		t.Fatalf("%d messages pinned", pinned)
	}
}

func TestPinLimit(t *testing.T) {
	a, fc, _, ctx := setup(t)
	onboard(t, a, fc, ctx)
	unhurried(a)
	for i := 0; i < store.MaxPinsPerConversation+1; i++ {
		a.Engine.HandleIncoming(ctx, dmID(fmt.Sprint("p", i), fmt.Sprint("note ", i)))
		fc.take()
		a.Engine.HandleIncoming(ctx, replyTo(fmt.Sprint("p", i), "pin this"))
		out := fc.take()
		if i < store.MaxPinsPerConversation && !strings.Contains(out[0].text, "Pinned") {
			t.Fatalf("pin %d refused: %+v", i, out)
		}
		if i == store.MaxPinsPerConversation && !strings.Contains(out[0].text, "most I keep") {
			t.Fatalf("pin over the limit: %+v", out)
		}
	}
}

func TestForgetRepliedMessage(t *testing.T) {
	a, fc, fm, ctx := setup(t)
	onboard(t, a, fc, ctx)
	a.Engine.HandleIncoming(ctx, dmID("m-oops", "my bank pin is 9182, oops"))
	fc.take()
	a.Engine.HandleIncoming(ctx, replyTo("m-oops", "forget this"))
	out := fc.take()
	if len(out) != 1 || !strings.Contains(out[0].text, "gone from our conversation") {
		t.Fatalf("forget acknowledgement: %+v", out)
	}
	if hasContent(convMessages(t, ctx, a), "9182") {
		t.Fatal("the message is still stored")
	}
	a.Engine.HandleIncoming(ctx, dm("what did I say?"))
	fc.take()
	if strings.Contains(allUserContent(lastChatReq(fm)), "9182") {
		t.Fatal("the forgotten message still reaches the model")
	}
	// A reply to a message that isn't stored.
	a.Engine.HandleIncoming(ctx, replyTo("nope", "forget this"))
	if out := fc.take(); len(out) != 1 || !strings.Contains(out[0].text, "don't have that message") {
		t.Fatalf("unknown message: %+v", out)
	}
}

func TestForgetSprigReplyDeletesItFromTheChat(t *testing.T) {
	a, fc, _, ctx := setup(t)
	onboard(t, a, fc, ctx)
	a.Engine.HandleIncoming(ctx, dm("tell me something"))
	fc.take()
	msgs := convMessages(t, ctx, a)
	last := msgs[len(msgs)-1]
	refs, _ := a.DB.MessageRefs(ctx, last.ID)
	if last.Role != "assistant" || len(refs) != 1 {
		t.Fatalf("sprig reply not tracked: %+v %v", last, refs)
	}
	scrubbed := len(fc.deleted) // the pasted key, during onboarding
	a.Engine.HandleIncoming(ctx, replyTo(refs[0], "delete this message"))
	fc.take()
	if len(fc.deleted) != scrubbed+1 || fc.deleted[scrubbed] != refs[0] {
		t.Fatalf("not deleted from the chat: %v", fc.deleted)
	}
	for _, m := range convMessages(t, ctx, a) {
		if m.ID == last.ID {
			t.Fatal("still in the conversation")
		}
	}
}

func TestDeleteThatRemovesTheLastSprigMessage(t *testing.T) {
	a, fc, _, ctx := setup(t)
	onboard(t, a, fc, ctx)
	a.Engine.HandleIncoming(ctx, dm("tell me something"))
	fc.take()
	before := convMessages(t, ctx, a)
	reply := before[len(before)-1]
	scrubbed := len(fc.deleted)
	a.Engine.HandleIncoming(ctx, dm("delete that"))
	if out := fc.take(); len(out) != 0 {
		t.Fatalf("delete that should just delete: %+v", out)
	}
	if len(fc.deleted) != scrubbed+1 {
		t.Fatalf("chat deletions: %v", fc.deleted)
	}
	after := convMessages(t, ctx, a)
	if len(after) != len(before)-1 || after[len(after)-1].ID == reply.ID {
		t.Fatalf("sprig reply not removed: %+v", after)
	}
}

func TestPlainForgetThatGoesToTheSprig(t *testing.T) {
	a, fc, fm, ctx := setup(t)
	onboard(t, a, fc, ctx)
	calls := fm.calls
	a.Engine.HandleIncoming(ctx, dm("forget that"))
	fc.take()
	if fm.calls == calls {
		t.Fatal(`"forget that" (about memories) was swallowed as a delete command`)
	}
}

func TestMessageDeletedInChatLeavesHistory(t *testing.T) {
	a, fc, fm, ctx := setup(t)
	onboard(t, a, fc, ctx)
	a.Engine.HandleIncoming(ctx, dmID("m-typo", "the meeting is at 3pm-TYPO"))
	fc.take()
	if fc.onDelete == nil {
		t.Fatal("the engine didn't register for deletions")
	}
	fc.onDelete(ctx, channel.Deletion{Channel: "discord", ChatID: "dm-owner", MessageIDs: []string{"m-typo", "unknown"}})
	if hasContent(convMessages(t, ctx, a), "TYPO") {
		t.Fatal("a message deleted in the chat app is still in the conversation")
	}
	a.Engine.HandleIncoming(ctx, dm("what time?"))
	fc.take()
	if strings.Contains(allUserContent(lastChatReq(fm)), "TYPO") {
		t.Fatal("the deleted message still reaches the model")
	}
}
