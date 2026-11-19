package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/store"
	"github.com/theshield2594/thicket/internal/websearch"
)

// kidDetails are personal details Max shares with his kid sprig in the
// containment test. None may leave the kid sandbox.
var kidDetails = []string{"Turner", "Elm Street", "Elm+Street", "Maple Grove", "555-201-7788", "5552017788", "max.t@example.com"}

// TestKidContainment is the promise in docs/household.md: what a kid tells
// their kid sprig stays with the kid (and their parents, who can read the
// whole chat). A conversation full of personal details, with every way out
// a parent can open (the web, an integration, family memory), then
// nothing of it may appear in any adult's private memory, in family
// memory, in an adult's chats or another channel, in a job, in the
// activity log, or in the arguments of any outbound call: web searches,
// fetched pages, integration calls. Memory extraction and conversation
// summaries included.
func TestKidContainment(t *testing.T) {
	h := setupHousehold(t)
	a, ctx := h.a, h.ctx
	day := fixDay(h)
	var sentAll []sent // everything sent while the kid talked, wherever it went
	// Every way out a parent can open: the web, an integration tool (in
	// the fixture), family memory.
	if _, err := a.Engine.KidAccess(ctx, h.owner, "bramble", []string{"web"}, nil, []string{"family_memory"}, nil); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var fetched []string
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		fetched = append(fetched, r.URL.String())
		mu.Unlock()
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<html><head><title>Dinosaurs</title></head><body><p>T. rex was big.</p></body></html>`))
	}))
	defer page.Close()
	a.Engine.Fetcher = &websearch.Fetcher{AllowPrivate: true}
	a.Settings.Set(ctx, "history_tokens", "300") // so the kid's conversation is summarised too
	// Memory review proposes the details for family memory and as the kid's own.
	h.fm.mu.Lock()
	h.fm.memoryReply = `{"ops":[{"op":"add","scope":"family","kind":"fact","content":"The family are the Turners at 42 Elm Street."},` +
		`{"op":"add","scope":"shared","kind":"person","content":"Max Turner goes to Maple Grove Elementary; mum's number is 555-201-7788."}]}`
	h.fm.mu.Unlock()

	for _, msg := range []string{
		"hi! my name is Max Turner",
		"I live at 42 Elm Street, and I go to Maple Grove Elementary",
		"my mum's number is 555-201-7788 and her email is max.t@example.com",
		"can you look up my school?",
		"open the map at " + page.URL,
		"check the light at my house",
		"remember we are vegetarian", // a family-scope memory, asked for by a kid
		strings.Repeat("I like dinosaurs a lot. ", 30),
	} {
		sentAll = append(sentAll, h.kidSays(msg)...)
	}
	// The guard stops only what carries details: these still go out.
	sentAll = append(sentAll, h.kidSays("look up dinosaurs")...)
	sentAll = append(sentAll, h.kidSays("open the page at "+page.URL)...)
	sentAll = append(sentAll, h.kidSays("check the light")...)

	// Memory review of the kid's (idle) conversation.
	a.DB.SetClock(func() time.Time { return day.Add(2 * time.Hour) })
	a.Reviewer.ReviewPending(ctx)
	a.DB.SetClock(func() time.Time { return day })
	if mems, _ := a.DB.AllMemories(ctx, h.kid.ID); len(mems) == 0 {
		t.Fatal("memory review didn't run on the kid's conversation (the test would prove nothing)")
	}

	leaks := func(where, s string) {
		t.Helper()
		for _, d := range kidDetails {
			if strings.Contains(strings.ToLower(s), strings.ToLower(d)) {
				t.Errorf("%q left the kid sandbox via %s: %s", d, where, s)
			}
		}
	}

	// Outbound calls: web searches (the search model), fetched pages, integration calls.
	var searches int
	h.fm.mu.Lock()
	for _, r := range h.fm.reqs {
		if strings.HasPrefix(r.Messages[0].Content, "You search the web") {
			searches++
			leaks("a web search", r.Messages[1].Content)
		}
	}
	h.fm.mu.Unlock()
	mu.Lock()
	for _, u := range fetched {
		leaks("a fetched page", u)
	}
	nFetched := len(fetched)
	mu.Unlock()
	haCalls.Lock()
	for _, b := range haCalls.bodies {
		leaks("an integration call", b)
	}
	nHA := len(haCalls.bodies)
	haCalls.Unlock()
	if searches != 1 || nFetched != 1 || nHA != 1 {
		t.Fatalf("the calls without details should have gone out once each: %d searches, %d pages, %d integration calls", searches, nFetched, nHA)
	}

	// Adults' private memory and family memory.
	q := func(query string, args ...any) []string {
		t.Helper()
		rows, err := a.DB.DB().QueryContext(ctx, query, args...)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			out = append(out, s)
		}
		return out
	}
	for _, s := range q("SELECT content FROM memories WHERE user_id != ?", h.kid.ID) {
		leaks("an adult's or family memory", s)
	}
	if fam, _ := a.DB.FamilyMemories(ctx, ""); len(fam) != 0 {
		t.Fatalf("a kid's conversation added to family memory: %+v", fam)
	}

	// Adults' chats, in any channel: only a parent's safety alert may quote the kid.
	for _, s := range q(`SELECT m.content FROM messages m JOIN conversations c ON c.id = m.conversation_id WHERE c.user_id != ?`, h.kid.ID) {
		if !strings.Contains(s, "Safety alert") {
			leaks("an adult's chat", s)
		}
	}
	for _, s := range q("SELECT summary FROM conversations WHERE user_id != ?", h.kid.ID) {
		leaks("an adult's conversation summary", s)
	}
	for _, s := range append(sentAll, h.fc.peek()...) {
		if s.chat != "dm-max" && !(s.chat == "dm-owner" && strings.Contains(s.text, "Safety alert")) {
			leaks("a message to "+s.chat, s.text)
		}
	}

	// Jobs, drafts, the activity log, and the trail outside the kid's own
	// safety entries (which only its parents see).
	for _, s := range q("SELECT name || ' ' || prompt FROM jobs") {
		leaks("a job", s)
	}
	for _, s := range q("SELECT title || ' ' || detail FROM inbox") {
		leaks("an inbox item", s)
	}
	for _, s := range q("SELECT summary || ' ' || detail FROM activity") {
		leaks("the activity log", s)
	}
	for _, s := range q("SELECT summary || ' ' || detail FROM trail WHERE NOT (kind = ? AND character = 'bramble')", store.TrailSafety) {
		leaks("the trail", s)
	}

	// A parent's report on the kid sprig: topics, with details taken out.
	r := reportText(t, h)
	leaks("the parent's report", r)
	if !strings.Contains(r, "dinosaurs") {
		t.Fatalf("report topics: %s", r)
	}
}

// fixDay fixes the database's and the engine's clocks at noon on Wednesday
// 17 March 2027 in the owner's timezone (after the test's accounts were
// made), so what's said lands on one known day (reportText reports it the
// next evening).
func fixDay(h *household) time.Time {
	day := time.Date(2027, 3, 17, 12, 0, 0, 0, h.owner.Location())
	h.a.DB.SetClock(func() time.Time { return day })
	h.a.Engine.Now = func() time.Time { return day }
	return day
}

// reportText runs the daily reports at 18:05 the day after fixDay's, and
// returns the owner's.
func reportText(t *testing.T, h *household) string {
	t.Helper()
	if err := h.a.Engine.SetKidReportFor(h.ctx, h.owner, store.KidReportDaily); err != nil {
		t.Fatal(err)
	}
	h.a.Settings.Set(h.ctx, "digest.time", "18:00")
	due := time.Date(2027, 3, 18, 18, 0, 0, 0, h.owner.Location())
	h.a.Engine.Now = func() time.Time { return due.Add(5 * time.Minute) }
	h.fc.take()
	h.a.Engine.DailyKidReports(h.ctx)
	var out []string
	for _, s := range h.fc.take() {
		if s.chat == "dm-owner" {
			out = append(out, s.text)
		}
	}
	if len(out) != 1 {
		t.Fatalf("daily report: %q", out)
	}
	return out[0]
}

// A parent sets time limits in chat or the web app; at a limit the kid
// sprig says so kindly and stops; the kid can't change them.
func TestKidTimeLimits(t *testing.T) {
	h := setupHousehold(t)
	a, ctx := h.a, h.ctx
	// Wednesday afternoon in the household's timezone.
	loc := h.kid.Location()
	now := time.Date(2026, 11, 18, 15, 0, 0, 0, loc)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	setClock := func(t time.Time) { mu.Lock(); now = t; mu.Unlock() }
	a.Engine.Now = clock
	a.DB.SetClock(clock)

	// Only its parents set them; never the kid, nor another adult.
	if _, err := a.Engine.SetKidLimits(ctx, h.kim, "bramble", character.KidLimits{DailyMessages: 50}); err == nil {
		t.Fatal("an adult who isn't its parent set its limits")
	}
	if _, err := a.Engine.SetKidLimits(ctx, h.kid, "bramble", character.KidLimits{}); err == nil {
		t.Fatal("the kid changed its limits")
	}
	h.kidSays("hi!")
	if toolNames(h.fm)["kid_limits"] {
		t.Fatal("a kid sprig was offered kid_limits")
	}

	// The parent, in chat with Juni.
	h.ownerSay("limit bramble to 3 messages a day")
	c, _ := a.Chars.Get("bramble")
	if c.Limits.DailyMessages != 3 || c.Limits.SchoolNights != "20:00" {
		t.Fatalf("limits from chat: %+v", c.Limits)
	}

	// Three answered messages today (the "hi!" was one), then a kind stop
	// with no model call.
	for i := 0; i < 2; i++ {
		setClock(now.Add(time.Minute))
		if out := h.kidSays("tell me about volcanoes"); strings.Contains(sentText(out), "all our chatting") {
			t.Fatalf("stopped early at message %d: %s", i+2, sentText(out))
		}
	}
	calls := h.fm.calls
	setClock(now.Add(time.Minute))
	out := h.kidSays("one more?")
	if len(out) != 1 || out[0].persona != "Bramble" || !strings.Contains(out[0].text, "That's all our chatting for today") {
		t.Fatalf("at the limit: %+v", out)
	}
	if h.fm.calls != calls {
		t.Fatal("the model was called past the limit")
	}
	// A kid in distress is answered, and their parent alerted.
	setClock(now.Add(time.Minute))
	out = h.kidSays("sometimes I want to die")
	var answered, alerted bool
	for _, s := range out {
		answered = answered || (s.chat == "dm-max" && s.persona == "Bramble" && s.text != "" && !strings.Contains(s.text, "all our chatting"))
		alerted = alerted || (s.chat == "dm-owner" && strings.Contains(s.text, "Safety alert"))
	}
	if !answered || !alerted || strings.Contains(sentText(out), "all our chatting") {
		t.Fatalf("a kid in distress at the limit: answered %v, parent alerted %v: %+v", answered, alerted, out)
	}
	// So is one the safety check flags (screened before the limit line).
	setClock(now.Add(time.Minute))
	out = h.kidSays("a guy online asked where I live and my school")
	answered, alerted = false, false
	for _, s := range out {
		answered = answered || (s.chat == "dm-max" && s.persona == "Bramble" && !strings.Contains(s.text, "all our chatting"))
		alerted = alerted || (s.chat == "dm-owner" && strings.Contains(s.text, "Safety alert"))
	}
	if !answered || !alerted {
		t.Fatalf("a flagged message at the limit: answered %v, parent alerted %v: %+v", answered, alerted, out)
	}
	// The stop is in the trail, not counted as a safety alert.
	kidTrail, _ := a.DB.Trail(ctx, h.kid.ID, 50)
	var stops int
	for _, e := range kidTrail {
		if e.Kind == store.TrailLimit && e.Character == "bramble" {
			stops++
		}
	}
	if stops != 1 {
		t.Fatalf("limit stops in the trail: %d", stops)
	}
	msgs, _ := a.Engine.KidChats(ctx, h.owner, "bramble", 0, 50)
	var shown bool
	for _, m := range msgs {
		shown = shown || strings.Contains(m.Content, "all our chatting")
	}
	if !shown {
		t.Fatal("the parent can't see the limit message")
	}

	// Limit lines don't use up the allowance: tries before chat hours
	// leave the whole day's messages.
	if _, err := a.Engine.SetKidLimits(ctx, h.owner, "bramble", character.KidLimits{Hours: "07:00-21:00", SchoolNights: "20:00", DailyMessages: 3}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		setClock(time.Date(2026, 11, 19, 6, 0, i, 0, loc))
		if out := h.kidSays("up yet?"); !strings.Contains(sentText(out), "not chat time") {
			t.Fatalf("before hours: %s", sentText(out))
		}
	}
	setClock(time.Date(2026, 11, 19, 7, 30, 0, 0, loc))
	if out := h.kidSays("good morning"); strings.Contains(sentText(out), "chatting for today") || strings.Contains(sentText(out), "not chat time") {
		t.Fatalf("tries before hours used up the allowance: %s", sentText(out))
	}

	// The next day the allowance is back, and a school night ends at 20:00.
	setClock(time.Date(2026, 11, 19, 19, 0, 0, 0, loc)) // Thursday
	if out := h.kidSays("hello again"); strings.Contains(sentText(out), "chatting for today") {
		t.Fatalf("yesterday's allowance carried over: %s", sentText(out))
	}
	setClock(time.Date(2026, 11, 19, 20, 30, 0, 0, loc))
	if out := h.kidSays("are you up?"); !strings.Contains(sentText(out), "school night") {
		t.Fatalf("school night: %s", sentText(out))
	}
	setClock(time.Date(2026, 11, 20, 20, 30, 0, 0, loc)) // Friday
	if out := h.kidSays("are you up?"); strings.Contains(sentText(out), "school night") {
		t.Fatalf("Friday isn't a school night: %s", sentText(out))
	}

	// In the trail, and the parent can undo it: the latest change first,
	// never an older one over a newer.
	trail, _ := a.DB.Trail(ctx, h.owner.ID, 20)
	var ids []int64 // newest first
	for _, e := range trail {
		if e.Character == "bramble" && strings.Contains(e.Summary, "time limits") {
			ids = append(ids, e.ID)
		}
	}
	if len(ids) != 2 {
		t.Fatalf("limit changes in the trail: %+v", trail)
	}
	if _, err := a.Engine.UndoTrail(ctx, h.kim, ids[0]); err == nil {
		t.Fatal("an adult who isn't its parent undid its limits")
	}
	if _, err := a.Engine.UndoTrail(ctx, h.owner, ids[1]); err == nil {
		t.Fatal("an older change was undone over a newer one")
	}
	if _, err := a.Engine.UndoTrail(ctx, h.owner, ids[0]); err != nil {
		t.Fatal(err)
	}
	if c, _ := a.Chars.Get("bramble"); c.Limits.DailyMessages != 3 || c.Limits.Hours != "" {
		t.Fatalf("undoing the latest change: %+v", c.Limits)
	}
	if _, err := a.Engine.UndoTrail(ctx, h.owner, ids[1]); err != nil {
		t.Fatal(err)
	}
	if c, _ := a.Chars.Get("bramble"); c.Limits.Any() {
		t.Fatalf("undo left limits: %+v", c.Limits)
	}
}

// Parents choose a daily report or one in the weekly digest: time spent,
// topics, anything flagged, what it cost.
func TestKidReports(t *testing.T) {
	h := setupHousehold(t)
	day := fixDay(h)
	h.kidSays("I love dinosaurs")
	h.kidSays("a guy online asked where I live and my school")
	r := reportText(t, h)
	for _, want := range []string{"Bramble with Max, on Wednesday 17 March", "Time: 1 minute, 2 messages", "Topics: Mostly dinosaurs", "[private]",
		"Flagged: 1 message by the safety check", "Cost: ", "Bramble's chats"} {
		if !strings.Contains(r, want) {
			t.Errorf("daily report lacks %q:\n%s", want, r)
		}
	}
	// Once a day: a later tick in the same window sends nothing more.
	h.a.Engine.Now = func() time.Time { return time.Date(2027, 3, 18, 18, 40, 0, 0, h.owner.Location()) }
	h.fc.take()
	h.a.Engine.DailyKidReports(h.ctx)
	for _, s := range h.fc.take() {
		if s.chat == "dm-owner" && strings.Contains(s.text, "Bramble with Max") {
			t.Fatalf("sent twice: %s", s.text)
		}
	}
	// Kim isn't Bramble's parent: no report, and she can't choose one about it.
	for _, s := range h.fc.peek() {
		if s.chat == "dm-kim" {
			t.Fatalf("Kim got %q", s.text)
		}
	}

	// Weekly, in the weekly digest.
	if err := h.a.Engine.SetKidReportFor(h.ctx, h.owner, store.KidReportWeekly); err != nil {
		t.Fatal(err)
	}
	h.a.Engine.Now = func() time.Time { return day.Add(time.Minute) }
	h.ownerSay("show me my weekly digest")
	if !modelSaw(h.fm, "Topics: Mostly dinosaurs") || !modelSaw(h.fm, "2 messages from Max") {
		t.Fatal("the weekly digest lacks the kid report")
	}
	if err := h.a.Engine.SetKidReportFor(h.ctx, h.kid, store.KidReportDaily); err == nil {
		t.Fatal("a kid chose a kid report")
	}
}
