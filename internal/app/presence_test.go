package app

import (
	"strings"
	"testing"
	"time"

	"github.com/theshield2594/thicket/internal/store"
)

// A person with Telegram, Discord and the web app gets proactive messages
// where they want them, with the others as fallbacks.
func TestProactiveMessagesFindThePerson(t *testing.T) {
	dc, tg := &fakeChannel{}, &fakeChannel{name: "telegram"}
	a, fm, ctx := setupWith(t, dc, tg)
	onboard(t, a, dc, ctx)
	owner, _ := a.DB.UserByExternal(ctx, "discord", "owner")
	daytime(t, a, owner.ID)
	a.Engine.HandleIncoming(ctx, dm("message me at most 20 a day")) // room for every brief below
	dc.take()
	for _, id := range []store.Identity{{Channel: "telegram", ExternalID: "tg-sam"}, {Channel: "web", ExternalID: "w-sam"}} {
		if err := a.DB.LinkIdentity(ctx, owner.ID, id.Channel, id.ExternalID); err != nil {
			t.Fatal(err)
		}
	}
	// Discord first (just now), then Telegram, then the web app last.
	a.DB.SetClock(func() time.Time { return time.Now().Add(time.Minute) })
	a.DB.UseIdentity(ctx, owner.ID, "telegram", "tg-sam")
	a.DB.SetClock(func() time.Time { return time.Now().Add(2 * time.Minute) })
	a.DB.UseIdentity(ctx, owner.ID, "web", "w-sam")
	a.DB.SetClock(time.Now)
	j, _ := a.DB.CreateJob(ctx, store.Job{UserID: owner.ID, Character: "juniper", Name: "Morning brief", Schedule: "0 8 * * *", Prompt: "Brief me."})
	webMsgs := func() int {
		n := 0
		for _, chat := range []string{"1.juniper"} {
			ms, _ := a.DB.WebMessages(ctx, chat, 0, 50)
			n += len(ms)
		}
		return n
	}

	// 1. The morning brief goes to Telegram, a phone app, though the web
	// app was used last.
	finishJob(ctx, a, j, "Good morning! Rain at noon.")
	if out := tg.take(); len(out) != 1 || !strings.Contains(out[0].text, "Rain at noon") {
		t.Fatalf("telegram: %+v", out)
	}
	if out := dc.take(); len(out) != 0 || webMsgs() != 0 {
		t.Fatalf("sent elsewhere too: discord %+v, web %d", out, webMsgs())
	}
	// The reply lands in the Telegram conversation.
	if _, err := a.DB.CurrentConversation(ctx, owner.ID, "telegram:dm-tg-sam", a.Cfg.IdleAfter); err != nil {
		t.Fatalf("no telegram conversation: %v", err)
	}

	// 2. Telegram failing falls back to Discord.
	tg.mu.Lock()
	tg.failIf = func(string) bool { return true }
	tg.mu.Unlock()
	finishJob(ctx, a, j, "Good morning! Sun all day.")
	if out := dc.take(); len(out) != 1 || !strings.Contains(out[0].text, "Sun all day") {
		t.Fatalf("discord fallback: %+v", out)
	}
	acts, _ := a.DB.RecentActivity(ctx, 20, "warn")
	if !hasActivity(acts, "delivered on Discord instead") {
		t.Fatalf("the fallback isn't in the activity log: %+v", acts)
	}

	// 3. Both phone apps failing: the web app is the last place to try
	// (and the inbox after that, as before).
	dc.mu.Lock()
	dc.failIf = func(s string) bool { return strings.Contains(s, "Snow") }
	dc.mu.Unlock()
	finishJob(ctx, a, j, "Good morning! Snow.")
	if ms, _ := a.DB.WebMessages(ctx, "1.juniper", 0, 10); len(ms) == 0 || !strings.Contains(ms[0].Content, "Snow") {
		t.Fatalf("web fallback: %+v", ms)
	}
	tg.mu.Lock()
	tg.failIf = nil
	tg.mu.Unlock()
	dc.mu.Lock()
	dc.failIf = nil
	dc.mu.Unlock()

	// 4. A chosen app wins ("reach me on discord"); one sprig can differ.
	a.Engine.HandleIncoming(ctx, dm("reach me on discord"))
	dc.take()
	if r := lastToolResult(fm); !strings.Contains(r, "reach them on Discord") {
		t.Fatalf("set_reach: %s", r)
	}
	finishJob(ctx, a, j, "Good morning! Fog.")
	if out := dc.take(); len(out) != 1 || !strings.Contains(out[0].text, "Fog") {
		t.Fatalf("chosen app: %+v", out)
	}
	if err := a.DB.SetReach(ctx, owner.ID, "juniper", "telegram"); err != nil {
		t.Fatal(err)
	}
	finishJob(ctx, a, j, "Good morning! Wind.")
	if out := tg.take(); len(out) != 1 || !strings.Contains(out[0].text, "Wind") {
		t.Fatalf("per-sprig override: %+v", out)
	}
	// An app they haven't linked can't be chosen.
	a.DB.SetReach(ctx, owner.ID, "juniper", "")
	if prefs, _ := a.DB.ReachPrefs(ctx, owner.ID); prefs["juniper"] != "" || prefs[""] != "discord" {
		t.Fatalf("prefs: %v", prefs)
	}
}

// Someone who only uses the web app is told once that a chat app would
// bring these messages to their phone.
func TestWebOnlyPersonIsToldAboutChatAppsOnce(t *testing.T) {
	a, fc, _, ctx := setup(t)
	onboard(t, a, fc, ctx)
	kim, err := a.DB.CreateUser(ctx, "web", "w-kim", "Kim", store.RoleMember)
	if err != nil {
		t.Fatal(err)
	}
	daytime(t, a, kim.ID)
	j, _ := a.DB.CreateJob(ctx, store.Job{UserID: kim.ID, Character: "juniper", Name: "Brief", Schedule: "0 8 * * *", Prompt: "Brief me."})
	chat := "2.juniper"
	finishJob(ctx, a, j, "Morning, Kim!")
	msgs, _ := a.DB.WebMessages(ctx, chat, 0, 10)
	if len(msgs) != 2 || msgs[0].Content != "Morning, Kim!" || !strings.Contains(msgs[1].Content, "link Discord") {
		t.Fatalf("web-only person: %+v", msgs)
	}
	finishJob(ctx, a, j, "Morning again!")
	if msgs, _ := a.DB.WebMessages(ctx, chat, 0, 10); len(msgs) != 3 {
		t.Fatalf("suggested twice: %+v", msgs)
	}
	if out := fc.take(); len(out) != 0 {
		t.Fatalf("sent to Discord, where Kim isn't: %+v", out)
	}
}
