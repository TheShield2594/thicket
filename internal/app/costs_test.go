package app

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/theshield2594/thicket/internal/agent"
	"github.com/theshield2594/thicket/internal/channel"
	"github.com/theshield2594/thicket/internal/llm"
	"github.com/theshield2594/thicket/internal/store"
)

func init() {
	files["https://cdn.example/manual.pdf"] = struct {
		body []byte
		ct   string
	}{[]byte("%PDF-1.4\n" + strings.Repeat("<< /Type /Page >>\n", 40)), "application/pdf"}
}

func manualPDF(text string) channel.Incoming {
	in := dm(text)
	in.Attachments = []channel.Attachment{{Name: "manual.pdf", URL: "https://cdn.example/manual.pdf", ContentType: "application/pdf", Size: 800}}
	return in
}

// A kid sprig with a $1 monthly cap stops at the cap: the kid hears a
// plain message, the parent gets a note once, and only the parent can
// raise the cap.
func TestKidSprigMonthlyCap(t *testing.T) {
	h := setupHousehold(t)
	a, ctx := h.a, h.ctx
	if _, err := a.Engine.SetSprigCap(ctx, h.owner, "bramble", "monthly", 1); err != nil {
		t.Fatal(err)
	}
	if out := h.kidSays("hi!"); personas(out) != "Bramble" {
		t.Fatalf("under the cap: %+v", out)
	}
	// Spend the rest of the month's dollar.
	if err := a.DB.RecordUsage(ctx, store.Usage{Character: "bramble", Model: "fake", CostUSD: 1}); err != nil {
		t.Fatal(err)
	}
	before := h.fm.calls
	out := h.kidSays("tell me a story")
	if h.fm.calls != before {
		t.Fatal("the model was called past the kid sprig's cap")
	}
	all := sentText(out)
	var kid, parent string
	for _, s := range out {
		switch s.chat {
		case "dm-max":
			kid += s.text
		case "dm-owner":
			parent += s.text
		}
	}
	if !strings.Contains(kid, "used up my allowance for this month") || strings.Contains(kid, "cap") || strings.Contains(kid, "$") {
		t.Fatalf("kid should get a plain message:\n%s", all)
	}
	if !strings.Contains(parent, "Bramble has used its monthly cap of $1.00") || !strings.Contains(parent, "only a parent can raise it") {
		t.Fatalf("parent should get a note:\n%s", all)
	}
	// Once per period, not on every message.
	if out := h.kidSays("please?"); strings.Contains(sentText(out), "has used its monthly cap") {
		t.Fatalf("parent told twice:\n%s", sentText(out))
	}

	// Kim (not a parent) can lower it but not raise it; the parent can.
	if _, err := a.Engine.SetSprigCap(ctx, h.kim, "bramble", "monthly", 5); err == nil {
		t.Fatal("a non-parent raised a kid sprig's cap")
	}
	if msg, err := a.Engine.SetSprigCap(ctx, h.owner, "bramble", "monthly", 2); err != nil || !strings.Contains(msg, "$2.00") {
		t.Fatalf("parent raise: %q %v", msg, err)
	}
	if out := h.kidSays("story time?"); personas(out) != "Bramble" || strings.Contains(sentText(out), "allowance") {
		t.Fatalf("after the raise: %+v", out)
	}
	// The change is in the trail, and undo puts the $1 back.
	trail, _ := a.DB.Trail(ctx, 0, 10)
	var raise *store.TrailEntry
	for i := range trail {
		if trail[i].Kind == store.TrailCap && strings.Contains(trail[i].Summary, "$1.00 → $2.00") {
			raise = &trail[i]
		}
	}
	if raise == nil || !raise.Undoable {
		t.Fatalf("raise not in the trail: %+v", trail)
	}
	if _, err := a.Engine.UndoTrail(ctx, h.owner, raise.ID); err != nil {
		t.Fatal(err)
	}
	if v := a.Settings.Float(ctx, "budget.character.bramble.monthly_usd"); v != 1 {
		t.Fatalf("undo left the cap at %v", v)
	}
}

// A new kid sprig starts with the kid cap.
func TestNewKidSprigStartsCapped(t *testing.T) {
	h := setupHousehold(t)
	c, err := h.a.Engine.CreateKidSprig(h.ctx, h.owner, agent.KidSprigSpec{ID: "moss", Name: "Moss", Role: "A reading buddy.", Kids: []int64{h.kid.ID}, Parents: []int64{h.owner.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if v := h.a.Settings.Float(h.ctx, "budget.character."+c.ID+".monthly_usd"); v != 1 {
		t.Fatalf("kid sprig cap: %v", v)
	}
}

// A PDF that would cost more than the threshold to OCR asks first, with
// the figure; "go ahead" reads it, and the estimate is checked against
// what it cost.
func TestCostlyPDFAsksFirst(t *testing.T) {
	a, fc, fm, ctx := setup(t)
	a.Engine.FetchAttachment = fakeFetch
	onboard(t, a, fc, ctx)
	for k, v := range map[string]string{"pdf_engine": "mistral-ocr", "budget.ask_above_usd": "0.10"} {
		if err := a.Settings.Set(ctx, k, v); err != nil {
			t.Fatal(err)
		}
	}
	// A short PDF is under the threshold: no question.
	a.Engine.HandleIncoming(ctx, pdfMessage("what's in this?"))
	if out := sentText(fc.take()); strings.Contains(out, "go ahead") {
		t.Fatalf("asked about a 2-page PDF:\n%s", out)
	}

	before := fm.calls
	a.Engine.HandleIncoming(ctx, manualPDF("summarise this manual"))
	out := sentText(fc.take())
	if fm.calls != before {
		t.Fatal("the model was called before the person agreed to the cost")
	}
	if !strings.Contains(out, "manual.pdf (40 pages) with OCR, about $0.08") || !strings.Contains(out, "will cost about $0.") || !strings.Contains(out, "go ahead") {
		t.Fatalf("want the estimate and a question:\n%s", out)
	}
	a.Engine.HandleIncoming(ctx, dm("go ahead"))
	fc.take()
	if req := lastChatReq(fm); !llmHasFile(req, "manual.pdf") {
		t.Fatal("the PDF wasn't sent after go ahead")
	}
	ests, _ := a.DB.Estimates(ctx, time.Now().Add(-time.Hour), 10)
	var got *store.Estimate
	for i := range ests {
		if ests[i].Kind == "pdf" && strings.Contains(ests[i].What, "manual.pdf") {
			got = &ests[i]
		}
	}
	if got == nil || got.Outcome != store.EstimateRan || !got.Finished || got.ActualUSD <= 0 {
		t.Fatalf("estimate vs actual not recorded: %+v", ests)
	}

	// "no" leaves it.
	a.Engine.HandleIncoming(ctx, manualPDF("and this one"))
	fc.take()
	before = fm.calls
	a.Engine.HandleIncoming(ctx, dm("no"))
	if out := sentText(fc.take()); !strings.Contains(out, "left it") || fm.calls != before {
		t.Fatalf("no: %s", out)
	}
}

// Over a cap, a costly action is refused, saying by how much.
func TestCostlyActionRefusedOverCap(t *testing.T) {
	a, fc, fm, ctx := setup(t)
	a.Engine.FetchAttachment = fakeFetch
	onboard(t, a, fc, ctx)
	owner, _ := a.DB.UserByExternal(ctx, "discord", "owner")
	if err := a.Settings.Set(ctx, "pdf_engine", "mistral-ocr"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Engine.SetSprigCap(ctx, owner, "juniper", "monthly", 0.05); err != nil {
		t.Fatal(err)
	}
	before := fm.calls
	a.Engine.HandleIncoming(ctx, manualPDF("summarise this manual"))
	out := sentText(fc.take())
	if fm.calls != before {
		t.Fatal("model called for a refused action")
	}
	if !strings.Contains(out, "I didn't start on that") || !strings.Contains(out, "more than Juni's monthly cap has left") {
		t.Fatalf("want a refusal with the figures:\n%s", out)
	}
}

func llmHasFile(req llm.Request, name string) bool {
	for _, m := range req.Messages {
		for _, f := range m.Files {
			if f.Name == name {
				return true
			}
		}
	}
	return false
}

// Review fixes: a cap can't be NaN or infinite, and undoing an older cap
// change doesn't overwrite a newer one.
func TestSprigCapEdgeCases(t *testing.T) {
	h := setupHousehold(t)
	a, ctx := h.a, h.ctx
	for _, bad := range []float64{math.NaN(), math.Inf(1)} {
		if _, err := a.Engine.SetSprigCap(ctx, h.owner, "bramble", "monthly", bad); err == nil {
			t.Fatalf("cap %v accepted", bad)
		}
	}
	a.Engine.SetSprigCap(ctx, h.owner, "bramble", "monthly", 2)
	a.Engine.SetSprigCap(ctx, h.owner, "bramble", "monthly", 0.5)
	trail, _ := a.DB.Trail(ctx, 0, 10)
	var first int64
	for _, e := range trail {
		if e.Kind == store.TrailCap && strings.Contains(e.Summary, "→ $2.00") {
			first = e.ID
		}
	}
	if _, err := a.Engine.UndoTrail(ctx, h.owner, first); err == nil || !strings.Contains(err.Error(), "changed again since") {
		t.Fatalf("undoing an older cap change: %v", err)
	}
	if v := a.Settings.Float(ctx, "budget.character.bramble.monthly_usd"); v != 0.5 {
		t.Fatalf("the newer cap was overwritten: %v", v)
	}
}

// A kid's request that's too costly for what's left of the cap is refused,
// and the parents hear it was turned down: not that the cap is used up
// (smaller things still work, and the real cap notice stays separate).
func TestKidEstimateRefusalWithHeadroom(t *testing.T) {
	h := setupHousehold(t)
	a, ctx := h.a, h.ctx
	a.Engine.FetchAttachment = fakeFetch
	if err := a.Settings.Set(ctx, "pdf_engine", "mistral-ocr"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Engine.SetSprigCap(ctx, h.owner, "bramble", "monthly", 0.05); err != nil {
		t.Fatal(err)
	}
	in := from("max", "dm-max", "read this to me")
	in.Attachments = []channel.Attachment{{Name: "manual.pdf", URL: "https://cdn.example/manual.pdf", ContentType: "application/pdf", Size: 800}}
	a.Engine.HandleIncoming(ctx, in)
	out := h.fc.take()
	var kid, parent string
	for _, s := range out {
		switch s.chat {
		case "dm-max":
			kid += s.text
		case "dm-owner":
			parent += s.text
		}
	}
	if !strings.Contains(kid, "too big a job for my allowance") {
		t.Fatalf("kid: %q", kid)
	}
	if !strings.Contains(parent, "Bramble turned down something Max asked for") || strings.Contains(parent, "resting until") {
		t.Fatalf("parent note should say it was turned down, not that the cap is used: %q", parent)
	}
	// A small message still gets an answer.
	if out := h.kidSays("hi"); personas(out) != "Bramble" || strings.Contains(sentText(out), "allowance") {
		t.Fatalf("small request after a refusal: %+v", out)
	}
}

// With the cap used up, a costly request is the cap reached: the kid hears
// the allowance message and the parents the cap note, not "turned down".
func TestKidEstimateRefusalAtExhaustedCap(t *testing.T) {
	h := setupHousehold(t)
	a, ctx := h.a, h.ctx
	a.Engine.FetchAttachment = fakeFetch
	if err := a.Settings.Set(ctx, "pdf_engine", "mistral-ocr"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Engine.SetSprigCap(ctx, h.owner, "bramble", "monthly", 1); err != nil {
		t.Fatal(err)
	}
	if err := a.DB.RecordUsage(ctx, store.Usage{Character: "bramble", Model: "fake", CostUSD: 1}); err != nil {
		t.Fatal(err)
	}
	in := from("max", "dm-max", "read this to me")
	in.Attachments = []channel.Attachment{{Name: "manual.pdf", URL: "https://cdn.example/manual.pdf", ContentType: "application/pdf", Size: 800}}
	a.Engine.HandleIncoming(ctx, in)
	var kid, parent string
	for _, s := range h.fc.take() {
		switch s.chat {
		case "dm-max":
			kid += s.text
		case "dm-owner":
			parent += s.text
		}
	}
	if !strings.Contains(kid, "used up my allowance") {
		t.Fatalf("kid: %q", kid)
	}
	if !strings.Contains(parent, "has used its monthly cap") || strings.Contains(parent, "smaller things") {
		t.Fatalf("parent should get the cap-reached note: %q", parent)
	}
}

// Only the latest change to a cap can be undone, even when a newer change
// set the same figure again; undoing newest-first walks all the way back.
func TestSprigCapUndoIsLatestFirst(t *testing.T) {
	h := setupHousehold(t)
	a, ctx := h.a, h.ctx
	for _, v := range []float64{1, 2, 0.5, 2} {
		if _, err := a.Engine.SetSprigCap(ctx, h.owner, "bramble", "monthly", v); err != nil {
			t.Fatal(err)
		}
	}
	cur := func() float64 { return a.Settings.Float(ctx, "budget.character.bramble.monthly_usd") }
	trail, _ := a.DB.TrailOfKind(ctx, store.TrailCap, "bramble", 10) // newest first
	if len(trail) != 4 {
		t.Fatalf("cap changes: %d", len(trail))
	}
	// $1 → $2 (an older change; the cap is $2 again, but not from it).
	if _, err := a.Engine.UndoTrail(ctx, h.owner, trail[2].ID); !errors.Is(err, agent.ErrUndoStale) || cur() != 2 {
		t.Fatalf("undoing an older change: %v, cap %v", err, cur())
	}
	for i, want := range []float64{0.5, 2, 1, 0} {
		if _, err := a.Engine.UndoTrail(ctx, h.owner, trail[i].ID); err != nil || cur() != want {
			t.Fatalf("undo %d: %v, cap %v (want %v)", i, err, cur(), want)
		}
	}
}
