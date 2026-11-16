package app

import (
	"regexp"
	"strings"
	"testing"

	"github.com/theshield2594/thicket/internal/channel"
)

var codeRe = regexp.MustCompile(`LINK-[A-Z0-9]{4}-[A-Z0-9]{4}`)

func tgDM(text string) channel.Incoming {
	return channel.Incoming{Channel: "telegram", ChatID: "777", MessageID: "m-" + text, UserID: "777", UserName: "Sam", Text: text, IsDM: true}
}

func TestLinkTelegramToDiscordProfile(t *testing.T) {
	dc, tg := &fakeChannel{}, &fakeChannel{name: "telegram"}
	a, fm, ctx := setupWith(t, dc, tg)
	e := a.Engine
	onboard(t, a, dc, ctx)
	owner, _ := a.DB.UserByExternal(ctx, "discord", "owner")

	// Before linking, the Telegram account is a stranger who's told how.
	e.HandleIncoming(ctx, tgDM("hi"))
	if out := tg.take(); len(out) != 1 || !strings.Contains(out[0].text, "link code") {
		t.Fatalf("stranger reply: %+v", out)
	}

	// Guessing fails, and stays quiet after a few tries.
	for i := 0; i < 7; i++ {
		e.HandleIncoming(ctx, tgDM("LINK-AAAA-AAAA"))
	}
	if out := tg.take(); len(out) != 5 {
		t.Fatalf("wrong-code replies: %d, want 5 then silence", len(out))
	}

	// Ask on Discord: the code is DMed by Thicket, not by the model.
	e.HandleIncoming(ctx, dm("link my other app please"))
	var code string
	for _, s := range dc.take() {
		code += codeRe.FindString(s.text)
	}
	if code == "" {
		t.Fatal("no code DMed")
	}
	fm.mu.Lock()
	for _, r := range fm.reqs {
		for _, m := range r.Messages {
			if strings.Contains(m.Content, code) {
				fm.mu.Unlock()
				t.Fatal("link code reached the model")
			}
		}
	}
	fm.mu.Unlock()
	assertNoLeak(t, a, code)

	// Redeem from a fresh Telegram account (rate limits are per account).
	in := tgDM("`" + strings.ToLower(code) + "`")
	in.UserID, in.ChatID = "778", "778"
	e.HandleIncoming(ctx, in)
	if out := tg.take(); len(out) != 1 || !strings.Contains(out[0].text, "Linked!") {
		t.Fatalf("redeem: %+v", out)
	}
	if u, err := a.DB.UserByExternal(ctx, "telegram", "778"); err != nil || u.ID != owner.ID {
		t.Fatalf("telegram account not linked to the owner: %+v %v", u, err)
	}
	// Single use.
	in2 := tgDM(code)
	in2.UserID, in2.ChatID = "779", "779"
	e.HandleIncoming(ctx, in2)
	if out := tg.take(); len(out) != 1 || !strings.Contains(out[0].text, "isn't valid") {
		t.Fatalf("reused code: %+v", out)
	}

	// Talking on Telegram now reaches the same sprigs and memories, and
	// proactive messages follow the user there.
	e.HandleIncoming(ctx, channel.Incoming{Channel: "telegram", ChatID: "778", MessageID: "x", UserID: "778", Text: "remember my dog is called Biscuit", IsDM: true})
	tg.take()
	if mems, _ := a.DB.AllMemories(ctx, owner.ID); len(mems) != 1 {
		t.Fatalf("memory from Telegram not on the shared profile: %+v", mems)
	}
	u, _ := a.DB.UserByID(ctx, owner.ID)
	if u.Channel != "telegram" || u.ExternalID != "778" {
		t.Fatalf("proactive messages should go to Telegram now: %+v", u)
	}
	e.Notify(ctx, "test", 0, "info", "ping")
	if out := tg.take(); len(out) != 1 || out[0].chat != "dm-778" {
		t.Fatalf("notification not sent on Telegram: %+v", out)
	}
}

func TestTelegramOnlyOwnerAndInvite(t *testing.T) {
	tg := &fakeChannel{name: "telegram"}
	a, _, ctx := setupWith(t, tg)
	e := a.Engine
	e.HandleIncoming(ctx, tgDM("hi"))
	if out := tg.take(); len(out) != 1 || !strings.Contains(out[0].text, "openrouter.ai/keys") {
		t.Fatalf("telegram-only bootstrap: %+v", out)
	}
	if u, err := a.DB.UserByExternal(ctx, "telegram", "777"); err != nil || u.Role != "owner" {
		t.Fatalf("first Telegram DM should make the owner: %+v %v", u, err)
	}
}
