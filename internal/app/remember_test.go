package app

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/theshield2594/thicket/internal/channel"
	"github.com/theshield2594/thicket/internal/secrets"
	"github.com/theshield2594/thicket/internal/store"
)

func tgIn(id, text string) channel.Incoming {
	return channel.Incoming{Channel: "telegram", ChatID: "tg-chat", MessageID: id, UserID: "tg-sam", UserName: "Sam", Text: text, IsDM: true}
}

func lockedMemory(t *testing.T, ctx context.Context, a *App, userID int64, content string) *store.Memory {
	t.Helper()
	mems, _ := a.DB.AllMemories(ctx, userID)
	fam, _ := a.DB.FamilyMemories(ctx, "")
	for _, m := range append(mems, fam...) {
		if strings.Contains(m.Content, content) {
			if !m.Locked || m.Source != "user" {
				t.Fatalf("memory isn't locked as the person's: %+v", m)
			}
			return &m
		}
	}
	t.Fatalf("no memory with %q in %+v", content, mems)
	return nil
}

func TestRememberThis(t *testing.T) {
	dc, tg := &fakeChannel{}, &fakeChannel{name: "telegram"}
	a, fm, ctx := setupWith(t, dc, tg)
	onboard(t, a, dc, ctx)
	unhurried(a)
	owner, _ := a.DB.UserByExternal(ctx, "discord", "owner")
	if err := a.DB.LinkIdentity(ctx, owner.ID, "telegram", "tg-sam"); err != nil {
		t.Fatal(err)
	}
	calls := func() int { fm.mu.Lock(); defer fm.mu.Unlock(); return fm.calls }

	// Telegram: reply "remember this". Handled in code, not by the model.
	a.Engine.HandleIncoming(ctx, tgIn("tg-1", "Our anniversary is June 3rd."))
	tg.take()
	before := calls()
	a.Engine.HandleIncoming(ctx, func() channel.Incoming { in := tgIn("tg-2", "remember this"); in.ReplyTo = "tg-1"; return in }())
	out := tg.take()
	if len(out) != 1 || !strings.Contains(out[0].text, "Remembered") || !strings.Contains(out[0].text, "June 3rd") ||
		!strings.Contains(out[0].text, "Just for you, or for the family?") {
		t.Fatalf("telegram confirmation: %+v", out)
	}
	if calls() != before {
		t.Fatal("remember this went to the model")
	}
	m := lockedMemory(t, ctx, a, owner.ID, "June 3rd")
	if !strings.HasSuffix(m.Content, " said: “Our anniversary is June 3rd.”") || m.Household {
		t.Fatalf("memory: %+v", m)
	}
	a.Engine.HandleIncoming(ctx, tgIn("tg-3", "just me"))
	if out := tg.take(); len(out) != 1 || !strings.Contains(out[0].text, "just for you") {
		t.Fatalf("answer: %+v", out)
	}
	if scope, _ := a.DB.RememberScope(ctx, owner.ID); scope != store.RememberPrivate {
		t.Fatalf("scope %q", scope)
	}

	// Discord: a reaction does the same, and isn't asked about again.
	a.Engine.HandleIncoming(ctx, dmID("d-1", "I take my coffee black."))
	dc.take()
	if dc.onReact == nil {
		t.Fatal("the engine didn't listen for Discord reactions")
	}
	dc.onReact(ctx, channel.Reaction{Channel: "discord", ChatID: "dm-owner", MessageID: "d-1", UserID: "owner", Emoji: "🧠", IsDM: true})
	out = dc.take()
	if len(out) != 1 || !strings.Contains(out[0].text, "coffee black") || strings.Contains(out[0].text, "family?") {
		t.Fatalf("reaction confirmation: %+v", out)
	}
	coffee := lockedMemory(t, ctx, a, owner.ID, "coffee black")
	ack := dc.lastPost()
	// Other reactions, and reactions by strangers, do nothing.
	dc.onReact(ctx, channel.Reaction{Channel: "discord", ChatID: "dm-owner", MessageID: "d-1", UserID: "owner", Emoji: "👍", IsDM: true})
	dc.onReact(ctx, channel.Reaction{Channel: "discord", ChatID: "dm-owner", MessageID: "d-1", UserID: "stranger", Emoji: "🧠", IsDM: true})
	if out := dc.take(); len(out) != 0 {
		t.Fatalf("other reactions: %+v", out)
	}

	// Both show up in list_memories.
	a.Engine.HandleIncoming(ctx, dm("what do you remember?"))
	dc.take()
	if r := lastToolResult(fm); !strings.Contains(r, "June 3rd") || !strings.Contains(r, "coffee black") || !strings.Contains(r, "you told me") {
		t.Fatalf("list_memories: %s", r)
	}

	// "forget this" on the confirmation undoes it.
	a.Engine.HandleIncoming(ctx, replyTo(ack, "forget this"))
	if out := dc.take(); len(out) != 1 || !strings.Contains(out[0].text, "Forgotten") {
		t.Fatalf("undo: %+v", out)
	}
	if _, err := a.DB.MemoryByID(ctx, coffee.ID); err == nil {
		t.Fatal("the memory is still there")
	}

	// With the text after it, and "for the family" overriding the answer.
	a.Engine.HandleIncoming(ctx, dm("remember this for the family: bins go out on Thursday"))
	if out := dc.take(); len(out) != 1 || !strings.Contains(out[0].text, "Remembered for the family") {
		t.Fatalf("family: %+v", out)
	}
	if m := lockedMemory(t, ctx, a, owner.ID, "bins go out"); !m.Household {
		t.Fatalf("not family memory: %+v", m)
	}

	// Plain "remember that I …" is still the sprig's to handle.
	before = calls()
	a.Engine.HandleIncoming(ctx, dm("remember that I hate olives"))
	dc.take()
	if calls() == before {
		t.Fatal("ordinary speech was taken as a command")
	}

}

func TestRememberAnswerFamily(t *testing.T) {
	a, fc, _, ctx := setup(t)
	onboard(t, a, fc, ctx)
	unhurried(a)
	owner, _ := a.DB.UserByExternal(ctx, "discord", "owner")
	a.Engine.HandleIncoming(ctx, dm("remember this: we're vegetarian now"))
	if out := fc.take(); len(out) != 1 || !strings.Contains(out[0].text, "family?") {
		t.Fatalf("question: %+v", out)
	}
	a.Engine.HandleIncoming(ctx, dm("for the family"))
	if out := fc.take(); len(out) != 1 || !strings.Contains(out[0].text, "Shared it with the family") {
		t.Fatalf("answer: %+v", out)
	}
	if m := lockedMemory(t, ctx, a, owner.ID, "vegetarian"); !m.Household {
		t.Fatalf("not moved to family memory: %+v", m)
	}
	// Next time it goes to the family without asking.
	a.Engine.HandleIncoming(ctx, dm("remember this: the cat is called Miso"))
	if out := fc.take(); len(out) != 1 || !strings.Contains(out[0].text, "Remembered for the family") || strings.Contains(out[0].text, "family?") {
		t.Fatalf("second: %+v", out)
	}
}

func TestKidRemembersPrivately(t *testing.T) {
	h := setupHousehold(t)
	out := h.kidSays("remember this: my favourite dinosaur is the stegosaurus")
	if len(out) != 1 || !strings.Contains(out[0].text, "Remembered") || strings.Contains(out[0].text, "family") {
		t.Fatalf("kid: %+v", out)
	}
	m := lockedMemory(t, h.ctx, h.a, h.kid.ID, "stegosaurus")
	if m.Household || m.UserID != h.kid.ID {
		t.Fatalf("kid memory: %+v", m)
	}
	// A kid can undo it, though messages stay put.
	h.a.Engine.HandleIncoming(h.ctx, func() channel.Incoming {
		in := from("max", "dm-max", "forget this")
		in.ReplyTo = h.fc.lastPost()
		return in
	}())
	if out := h.fc.take(); len(out) != 1 || !strings.Contains(out[0].text, "Forgotten") {
		t.Fatalf("kid undo: %+v", out)
	}
}

func TestRememberButtonInWebApp(t *testing.T) {
	a, _, _, ctx := setup(t)
	if err := os.WriteFile(filepath.Join(a.Cfg.CharactersDir, "pip.md"), []byte(pip), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.Chars.Load(); err != nil {
		t.Fatal(err)
	}
	if err := a.Secrets.Put(ctx, secrets.OpenRouterKey, "sk-or-test"); err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	go a.Web.Run(runCtx, a.Engine.HandleIncoming)
	waitFor(t, "web adapter", a.Web.Connected)
	srv := httptest.NewServer(a.Web.Handler())
	defer srv.Close()
	w := signIn(t, a, ctx, srv.URL)
	owner, _ := a.DB.Owner(ctx)
	pipChat := fmt.Sprintf("%d.pip", owner.ID)
	w.do("POST", "/api/chats/"+pipChat+"/messages", map[string]string{"text": "My locker is number 12."}, true)
	var msgs []store.WebMessage
	waitFor(t, "Pip's reply", func() bool {
		msgs, _ = a.DB.WebMessages(ctx, pipChat, 0, 10)
		return len(msgs) == 2
	})
	code, out := w.do("POST", fmt.Sprintf("/api/chats/%s/messages/%d/remember", pipChat, msgs[0].ID), nil, true)
	if code != 200 || !strings.Contains(fmt.Sprint(out["note"]), "Remembered") {
		t.Fatalf("remember button: %d %v", code, out)
	}
	lockedMemory(t, ctx, a, owner.ID, "locker is number 12")
	if code, _ := w.do("POST", fmt.Sprintf("/api/chats/%d.pip/messages/%d/remember", owner.ID+1, msgs[0].ID), nil, true); code != 404 {
		t.Fatalf("someone else's chat: %d", code)
	}
	// The sprig's confirmation is in the thread too.
	waitFor(t, "confirmation", func() bool {
		msgs, _ = a.DB.WebMessages(ctx, pipChat, 0, 10)
		return len(msgs) == 3 && strings.Contains(msgs[2].Content, "Remembered")
	})
}
