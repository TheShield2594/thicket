package app

import (
	"context"
	"fmt"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/theshield2594/thicket/internal/auth"
	"github.com/theshield2594/thicket/internal/memory"
	"github.com/theshield2594/thicket/internal/store"
	"github.com/theshield2594/thicket/internal/web"
)

func memoryInput(sprig, kind, content string) memory.Input {
	return memory.Input{Sprig: sprig, Kind: kind, Content: content}
}

// lastPromptOf is the newest system prompt a sprig answered with.
func lastPromptOf(fm *fakeModel, name string) string {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	for i := len(fm.reqs) - 1; i >= 0; i-- {
		if sys := fm.reqs[i].Messages[0].Content; strings.HasPrefix(sys, "You are "+name) {
			return sys
		}
	}
	return ""
}

// relSection is the prompt's "how to be with this person" section.
func relSection(sys string) string {
	i := strings.Index(sys, "## How to be with this person")
	if i < 0 {
		return ""
	}
	j := strings.Index(sys[i:], "## What you remember")
	return sys[i : i+j]
}

// The memory screen over API v2: a person sees relationship memory apart
// from facts, edits it, and the next reply uses the edit. Everyone reaches
// only their own, and a kid sprig's parents reach what it remembers.
func TestMemoryScreenAPI(t *testing.T) {
	h := setupHousehold(t)
	a, ctx := h.a, h.ctx
	for u, name := range map[*store.User]string{h.owner: "sam", h.kim: "kim", h.kid: "max"} {
		hash, _ := auth.HashPassword(name + " long password")
		if err := a.DB.SetLogin(ctx, u.ID, name, hash); err != nil {
			t.Fatal(err)
		}
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	go a.Web.Run(runCtx, a.Engine.HandleIncoming)
	waitFor(t, "web adapter", a.Web.Connected)
	srv := httptest.NewServer(a.Web.V2Handler())
	defer srv.Close()
	anon := &v2client{t: t, base: srv.URL}
	login := func(user string) *v2client {
		t.Helper()
		code, out := anon.call("POST", "/api/v2/auth/login", map[string]string{"username": user, "password": user + " long password"})
		if code != 200 {
			t.Fatalf("login %s: %d %v", user, code, out)
		}
		return &v2client{t: t, base: srv.URL, token: out["token"].(string)}
	}
	owner, kim, kid := login("sam"), login("kim"), login("max")
	if code, _ := anon.call("GET", "/api/v2/memories", nil); code != 401 {
		t.Fatalf("no token: %d", code)
	}
	mem := func(out map[string]any) map[string]any {
		t.Helper()
		m, _ := out["memory"].(map[string]any)
		if m == nil {
			t.Fatalf("no memory in %v", out)
		}
		return m
	}
	list := func(c *v2client, q string) (facts, rel []map[string]any) {
		t.Helper()
		code, out := c.call("GET", "/api/v2/memories"+q, nil)
		if code != 200 {
			t.Fatalf("list %s: %d %v", q, code, out)
		}
		for _, x := range out["facts"].([]any) {
			facts = append(facts, x.(map[string]any))
		}
		for _, x := range out["relationship"].([]any) {
			rel = append(rel, x.(map[string]any))
		}
		return facts, rel
	}

	// Add: relationship memory is with one sprig; facts may be shared.
	code, out := owner.call("POST", "/api/v2/memories", map[string]any{"sprig": "juniper", "kind": "tone", "content": "Prefers short answers, no emoji."})
	if code != 201 {
		t.Fatalf("add tone: %d %v", code, out)
	}
	tone := mem(out)
	if tone["group"] != "relationship" || tone["locked"] != true || tone["from"] != "you" || tone["sprig_name"] != "Juni" {
		t.Fatalf("added tone: %v", tone)
	}
	toneID := int64(tone["id"].(float64))
	if code, out := owner.call("POST", "/api/v2/memories", map[string]any{"kind": "fact", "content": "Has a dog named Biscuit."}); code != 201 || mem(out)["group"] != "facts" {
		t.Fatalf("add fact: %d %v", code, out)
	}
	if code, _ := owner.call("POST", "/api/v2/memories", map[string]any{"kind": "dislikes", "content": "Hates being asked twice."}); code != 400 {
		t.Fatalf("relationship memory without a sprig: %d", code)
	}
	if code, _ := owner.call("POST", "/api/v2/memories", map[string]any{"kind": "followup", "content": "Ask about the interview."}); code != 400 {
		t.Fatalf("follow-up from the screen: %d", code)
	}
	facts, rel := list(owner, "")
	if len(facts) != 1 || len(rel) != 1 || rel[0]["content"] != "Prefers short answers, no emoji." {
		t.Fatalf("list: facts %v, relationship %v", facts, rel)
	}

	// The next reply is shaped by it, as how to be with them, not a fact.
	h.ownerSay("hello Juni")
	sys := lastPromptOf(h.fm, "Juni")
	if !strings.Contains(relSection(sys), "Prefers short answers, no emoji.") {
		t.Fatalf("tone not in the prompt's relationship section:\n%s", sys)
	}

	// Edit it: undoable, and the very next reply follows the edit.
	code, out = owner.call("PATCH", fmt.Sprintf("/api/v2/memories/%d", toneID), map[string]any{"content": "Likes long, detailed answers."})
	if code != 200 || mem(out)["content"] != "Likes long, detailed answers." || mem(out)["undoable"] != true || mem(out)["locked"] != true {
		t.Fatalf("edit: %d %v", code, out)
	}
	h.ownerSay("hello again")
	if rs := relSection(lastPromptOf(h.fm, "Juni")); !strings.Contains(rs, "Likes long, detailed answers.") || strings.Contains(rs, "Prefers short answers") {
		t.Fatalf("edit not in the next prompt:\n%s", rs)
	}
	// Lock and unlock go through the history too; undo walks back.
	if code, out := owner.call("PUT", fmt.Sprintf("/api/v2/memories/%d/lock", toneID), map[string]bool{"locked": false}); code != 200 || mem(out)["locked"] != false {
		t.Fatalf("unlock: %d %v", code, out)
	}
	if code, out := owner.call("POST", fmt.Sprintf("/api/v2/memories/%d/undo", toneID), nil); code != 200 || mem(out)["locked"] != true {
		t.Fatalf("undo unlock: %d %v", code, out)
	}
	if code, out := owner.call("POST", fmt.Sprintf("/api/v2/memories/%d/undo", toneID), nil); code != 200 || mem(out)["content"] != "Prefers short answers, no emoji." {
		t.Fatalf("undo edit: %d %v", code, out)
	}

	// Nobody else reaches it.
	if facts, rel := list(kim, ""); len(facts)+len(rel) != 0 {
		t.Fatalf("Kim sees someone else's memories: %v %v", facts, rel)
	}
	for _, c := range []*v2client{kim, kid} {
		if code, _ := c.call("PATCH", fmt.Sprintf("/api/v2/memories/%d", toneID), map[string]any{"content": "Hijacked."}); code != 404 {
			t.Fatalf("someone else edited it: %d", code)
		}
		if code, _ := c.call("DELETE", fmt.Sprintf("/api/v2/memories/%d", toneID), nil); code != 404 {
			t.Fatalf("someone else deleted it: %d", code)
		}
		if code, _ := c.call("POST", fmt.Sprintf("/api/v2/memories/%d/undo", toneID), nil); code != 404 {
			t.Fatalf("someone else undid it: %d", code)
		}
	}

	// Kid sprigs: relationship memory stays in the sandbox. The kid keeps
	// theirs with their kid sprig; parents see and change it; other adults
	// and adult sprigs never do.
	if code, _ := kid.call("POST", "/api/v2/memories", map[string]any{"sprig": "juniper", "kind": "likes", "content": "Likes Juni."}); code != 400 {
		t.Fatalf("kid added memory with an adult sprig: %d", code)
	}
	code, out = kid.call("POST", "/api/v2/memories", map[string]any{"sprig": "bramble", "kind": "likes", "content": "Loves dinosaur facts."})
	if code != 201 {
		t.Fatalf("kid add: %d %v", code, out)
	}
	kidMemID := int64(mem(out)["id"].(float64))
	_, rel = list(owner, "?sprig=bramble")
	if len(rel) != 1 || rel[0]["content"] != "Loves dinosaur facts." || rel[0]["person_name"] != "Max" || rel[0]["from"] != "Max" {
		t.Fatalf("parent's view of the kid sprig: %v", rel)
	}
	if code, _ := kim.call("GET", "/api/v2/memories?sprig=bramble", nil); code != 403 {
		t.Fatalf("non-parent read a kid sprig's memory: %d", code)
	}
	if _, rel := list(owner, ""); len(rel) != 1 {
		t.Fatalf("kid's memory leaked into the parent's own: %v", rel)
	}
	if code, out := owner.call("PATCH", fmt.Sprintf("/api/v2/memories/%d", kidMemID), map[string]any{"content": "Loves dinosaur and volcano facts."}); code != 200 {
		t.Fatalf("parent edit: %d %v", code, out)
	}
	code, out = owner.call("POST", "/api/v2/memories", map[string]any{"sprig": "bramble", "kind": "tone", "content": "Likes simple words."})
	if code != 201 || int64(mem(out)["person"].(float64)) != h.kid.ID {
		t.Fatalf("parent add to the kid sprig: %d %v", code, out)
	}
	if code, _ := kim.call("PATCH", fmt.Sprintf("/api/v2/memories/%d", kidMemID), map[string]any{"content": "x y z"}); code != 404 {
		t.Fatalf("non-parent edited a kid's memory: %d", code)
	}
	_, rel = list(kid, "")
	if len(rel) != 2 {
		t.Fatalf("kid's own view: %v", rel)
	}
	h.kidSays("tell me a fact")
	if rs := relSection(lastPromptOf(h.fm, "Bramble")); !strings.Contains(rs, "Loves dinosaur and volcano facts.") || !strings.Contains(rs, "Likes simple words.") {
		t.Fatalf("kid sprig's prompt:\n%s", rs)
	}

	// Delete forgets for good: nothing to undo afterwards.
	if code, _ := owner.call("DELETE", fmt.Sprintf("/api/v2/memories/%d", toneID), nil); code != 200 {
		t.Fatalf("delete: %d", code)
	}
	if code, _ := owner.call("POST", fmt.Sprintf("/api/v2/memories/%d/undo", toneID), nil); code != 404 {
		t.Fatalf("undo after delete: %d", code)
	}
	if _, rel := list(owner, ""); len(rel) != 0 {
		t.Fatalf("deleted memory still listed: %v", rel)
	}
}

// The dashboard's memory screen: relationship apart from facts, with who
// and when, and every entry editable, lockable, undoable and deletable.
func TestMemoryScreenDashboard(t *testing.T) {
	h := setupHousehold(t)
	a, ctx := h.a, h.ctx
	tone, err := a.Engine.AddMemory(ctx, h.owner, memoryInput("juniper", "tone", "Prefers short answers, no emoji."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Engine.AddMemory(ctx, h.kid, memoryInput("bramble", "likes", "Loves dinosaur facts.")); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a.Dashboard.Handler())
	defer srv.Close()
	session, _ := a.DB.CreateWebSession(ctx, h.owner.ID, "test", web.SessionTTL)
	_, body := getDashboard(t, srv, session)
	rel, facts := strings.Index(body, "<h3>Relationship</h3>"), strings.Index(body, "<h3>Facts</h3>")
	if rel < 0 || facts < rel || !strings.Contains(body[rel:facts], "Prefers short answers, no emoji.") || !strings.Contains(body[rel:facts], "From you") {
		t.Fatalf("relationship memory not shown apart from facts:\n%s", body)
	}
	if !strings.Contains(body, "What Bramble remembers about Max") || !strings.Contains(body, "Loves dinosaur facts.") {
		t.Fatal("parent doesn't see the kid sprig's memory")
	}
	id := fmt.Sprint(tone.ID)
	act := func(action string, extra ...string) {
		t.Helper()
		form := url.Values{"csrf": {csrfFrom(t, body)}, "kind": {"memory"}, "action": {action}, "id": {id}}
		for i := 0; i+1 < len(extra); i += 2 {
			form.Set(extra[i], extra[i+1])
		}
		if code, out := postAct(t, srv, session, form); code != 303 {
			t.Fatalf("%s: %d %s", action, code, out)
		}
	}
	act("edit", "content", "Likes long, detailed answers.", "mkind", "tone")
	if m, _ := a.DB.MemoryByID(ctx, tone.ID); m.Content != "Likes long, detailed answers." || !m.Locked {
		t.Fatalf("edit: %+v", m)
	}
	_, body = getDashboard(t, srv, session)
	if !strings.Contains(body, `value="undo"`) {
		t.Fatal("no undo after an edit")
	}
	act("unlock")
	if m, _ := a.DB.MemoryByID(ctx, tone.ID); m.Locked {
		t.Fatal("unlock")
	}
	act("undo")
	act("undo")
	if m, _ := a.DB.MemoryByID(ctx, tone.ID); m.Content != "Prefers short answers, no emoji." || !m.Locked {
		t.Fatalf("undo: %+v", m)
	}
	act("delete")
	if _, err := a.DB.MemoryByID(ctx, tone.ID); err == nil {
		t.Fatal("delete")
	}

	// Kim sees neither, and can't act on them.
	ks, _ := a.DB.CreateWebSession(ctx, h.kim.ID, "test", web.SessionTTL)
	_, kbody := getDashboard(t, srv, ks)
	if strings.Contains(kbody, "dinosaur") || strings.Contains(kbody, "Bramble remembers") {
		t.Fatal("Kim sees the kid sprig's memory")
	}
}
