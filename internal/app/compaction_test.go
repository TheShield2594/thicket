package app

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// chatter sends n distinct, longish messages.
func chatter(t *testing.T, ctx context.Context, a *App, fc *fakeChannel, from, n int) {
	t.Helper()
	for i := from; i < from+n; i++ {
		a.Engine.HandleIncoming(ctx, dm(fmt.Sprintf("topic-%02d %s", i, strings.Repeat("words about it ", 20))))
		fc.take()
	}
}

func TestLongConversationIsCompacted(t *testing.T) {
	a, fc, fm, ctx := setup(t)
	onboard(t, a, fc, ctx)
	a.Settings.Set(ctx, "history_tokens", "600") // 2,400 characters

	chatter(t, ctx, a, fc, 1, 12)
	if fm.compactions == 0 {
		t.Fatal("a conversation over budget was never summarised")
	}
	owner, _ := a.DB.UserByExternal(ctx, "discord", "owner")
	conv, _ := a.DB.OpenConversation(ctx, owner.ID, "discord:dm-owner", a.Cfg.IdleAfter)
	if !strings.Contains(conv.Summary, "SUMMARY-NOTES") || conv.SummaryUpto == 0 {
		t.Fatalf("summary not stored: %+v", conv)
	}

	// The next reply sees the summary, not the oldest messages, and stays
	// within budget.
	a.Engine.HandleIncoming(ctx, dm("what was the first topic?"))
	fc.take()
	req := lastChatReq(fm)
	got := allUserContent(req)
	if !strings.Contains(got, "Thicket's summary of them") || !strings.Contains(got, "SUMMARY-NOTES") {
		t.Fatalf("summary missing from the prompt:\n%s", got)
	}
	if strings.Contains(got, "topic-01 ") {
		t.Fatal("the oldest message is still sent verbatim")
	}
	n := 0
	for _, m := range req.Messages[1:] {
		n += len(m.Content)
	}
	if n > 2400+1500 { // budget plus the summary and framing
		t.Fatalf("history is %d characters, over budget", n)
	}
	// Memory review still sees the whole conversation, not the summary.
	if msgs, _ := a.DB.RecentMessages(ctx, conv.ID, 200); !strings.Contains(msgs[0].Content+msgs[1].Content+msgs[2].Content, "topic-01") {
		t.Fatal("raw messages were lost")
	}
}

func TestCompactionFailureDropsOldMessages(t *testing.T) {
	a, fc, fm, ctx := setup(t)
	onboard(t, a, fc, ctx)
	a.Settings.Set(ctx, "history_tokens", "600")
	fm.mu.Lock()
	fm.compactFail = true
	fm.mu.Unlock()
	chatter(t, ctx, a, fc, 1, 12)
	a.Engine.HandleIncoming(ctx, dm("still with me?"))
	out := fc.take()
	if len(out) == 0 || strings.Contains(out[0].text, "went wrong") {
		t.Fatalf("replies must not depend on summarising: %+v", out)
	}
	got := allUserContent(lastChatReq(fm))
	if strings.Contains(got, "topic-01 ") || strings.Contains(got, "summary of them") {
		t.Fatalf("without a summary, old messages should just drop out:\n%s", got)
	}
}

func TestContextOverflowRetriesWithLessHistory(t *testing.T) {
	a, fc, fm, ctx := setup(t)
	onboard(t, a, fc, ctx)
	chatter(t, ctx, a, fc, 1, 6) // fits the automatic budget, ~2,000 chars
	fm.mu.Lock()
	fm.contextLimit = 1800 // a small model
	fm.compactFail = true  // so only the retry can save the turn
	fm.mu.Unlock()
	a.Engine.HandleIncoming(ctx, dm("and now?"))
	var reply, notice bool
	for _, s := range fc.take() {
		reply = reply || strings.Contains(s.text, "Lovely to meet you")
		notice = notice || strings.Contains(s.text, "ran out of context")
	}
	if !reply || !notice {
		t.Fatal("want a reply after trimming history, and a notice")
	}
}
