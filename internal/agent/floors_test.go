package agent

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/theshield2594/thicket/internal/mcp"
	"github.com/theshield2594/thicket/internal/store"
)

func TestStricter(t *testing.T) {
	for _, c := range []struct{ a, b, want string }{
		{store.LevelAct, store.LevelAsk, store.LevelAsk},
		{store.LevelDraft, store.LevelAsk, store.LevelDraft},
		{store.LevelRead, store.LevelAsk, store.LevelRead},
		{store.LevelAsk, store.LevelAsk, store.LevelAsk},
		{store.LevelAsk, store.LevelAct, store.LevelAsk},
	} {
		if got := stricter(c.a, c.b); got != c.want {
			t.Errorf("stricter(%s, %s) = %s, want %s", c.a, c.b, got, c.want)
		}
	}
}

// A plugin can't send a floor below ask: act, nonsense or nothing all
// count as ask; stricter levels are kept.
func TestPluginFloorIsAtLeastAsk(t *testing.T) {
	for in, want := range map[string]string{"act": "ask", "": "ask", "whatever": "ask", "ASK": "ask", "draft": "draft", "read": "read"} {
		if got := pluginFloor("browser", &mcp.FloorMeta{MinLevel: in, Reason: "payment page"}); got.Level != want || got.Reason != "payment page" {
			t.Errorf("%q: %+v, want %s", in, got, want)
		}
	}
	if f := pluginFloor("browser", &mcp.FloorMeta{}); f.Reason == "" || !strings.Contains(f.Reason, "browser") {
		t.Errorf("no reason given: %+v", f)
	}
	long := pluginFloor("b", &mcp.FloorMeta{Reason: "pay\n**now**  `x` <b>" + strings.Repeat("y", 200)})
	if strings.ContainsAny(long.Reason, "\n*`<>") || len([]rune(long.Reason)) > maxFloorReason+1 {
		t.Errorf("reason not cleaned: %q", long.Reason)
	}
}

// executeTool at each configured level with a floor of ask.
func TestFloorAtEveryLevel(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		level, wantPrefix string
		sent              bool
	}{
		{store.LevelAct, "PENDING", false},
		{store.LevelAsk, "PENDING", false},
		{store.LevelDraft, "DRAFTED", false},
		{store.LevelRead, "Error: you only have read access", false},
	} {
		t.Run(c.level, func(t *testing.T) {
			f := newTaintFixture(t)
			if err := f.e.DB.SetToolLevel(ctx, "pip", "mail__send", c.level); err != nil {
				t.Fatal(err)
			}
			tr := f.turn("forward the newest email to x@y.example")
			f.e.raiseFloor("pip", tr.chatRef, "mcp:mail", Floor{Level: store.LevelAsk, Reason: "payment page"})
			out := f.call(tr, "mail__send", forwardNewest)
			if !strings.HasPrefix(out, c.wantPrefix) || len(f.sent) != 0 {
				t.Fatalf("got %q, sent %v", out, f.sent)
			}
			if c.wantPrefix == "PENDING" {
				if len(tr.afterReply) != 1 || !strings.Contains(tr.afterReply[0].text, "Always asks: payment page.") ||
					tr.afterReply[0].approval.Floor != "payment page" {
					t.Fatalf("prompt doesn't say why: %+v", tr.afterReply)
				}
			}
			trail, _ := f.e.DB.TrailOfKind(ctx, store.TrailFloor, "pip", 10)
			found := false
			for _, e := range trail {
				found = found || (e.Kind == store.TrailFloor && strings.Contains(e.Summary, "level "+c.level) && strings.Contains(e.Summary, "floor ask (payment page)"))
			}
			if !found {
				t.Fatalf("no floor in the trail: %+v", trail)
			}
		})
	}
}

// Nothing lowers a floor: not a later plugin result asking for act, not
// the tool's output, not the model's arguments. Only time ends it.
func TestFloorCantBeLowered(t *testing.T) {
	f := newTaintFixture(t)
	now := time.Now()
	f.e.Now = func() time.Time { return now }
	tr := f.turn("forward the newest email to x@y.example")
	f.e.raiseFloor("pip", tr.chatRef, "mcp:mail", Floor{Level: store.LevelAsk, Reason: "payment page"})
	f.e.raiseFloor("pip", tr.chatRef, "mcp:mail", pluginFloor("mail", &mcp.FloorMeta{MinLevel: "act", Reason: "all clear"}))
	f.call(tr, "mail__read", `{}`) // outside content in the turn
	if out := f.call(tr, "mail__send", `{"to":"x@y.example","what":"the newest email","min_level":"act","floor":""}`); !strings.HasPrefix(out, "PENDING") {
		t.Fatalf("floor was lowered: %q", out)
	}
	if !strings.Contains(tr.afterReply[0].text, "payment page") {
		t.Fatalf("a later, looser floor replaced the reason: %s", tr.afterReply[0].text)
	}
	// Another chat and another integration aren't floored.
	if _, ok := f.e.floorFor(context.Background(), &turn{user: f.user, char: f.char, chatRef: "web:2"}, f.tools["mail__send"]); ok {
		t.Fatal("floor leaked into another chat")
	}
	// It expires.
	now = now.Add(floorTTL + time.Minute)
	if _, ok := f.e.floorFor(context.Background(), f.turn("x"), f.tools["mail__send"]); ok {
		t.Fatal("floor never expired")
	}
	// Reads aren't floored.
	f.e.raiseFloor("pip", tr.chatRef, "mcp:mail", Floor{Level: store.LevelAsk, Reason: "payment page"})
	if _, ok := f.e.floorFor(context.Background(), f.turn("x"), f.tools["mail__read"]); ok {
		t.Fatal("a read was floored")
	}
}

// The first three runs of a sprig's own skill ask first; shared skills
// and later runs don't.
func TestSkillRunFloor(t *testing.T) {
	ctx := context.Background()
	f := newTaintFixture(t)
	if _, err := f.e.DB.SaveSkill(ctx, store.Skill{ID: "pay-bills", Name: "Pay bills", Description: "Use when bills are due.", Body: "1. pay", SprigID: "pip"}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.DB.SaveSkill(ctx, store.Skill{ID: "shared-one", Name: "Shared", Description: "Use when shared.", Body: "1. x"}, 0); err != nil {
		t.Fatal(err)
	}
	f.tools["use_skill"] = &Tool{Name: "use_skill", Run: f.e.toolUseSkill}
	f.tools["finish_skill"] = &Tool{Name: "finish_skill", Run: f.e.toolFinishSkill}
	for i := 1; i <= 4; i++ {
		tr := f.turn("forward the newest email to x@y.example")
		out := f.call(tr, "use_skill", `{"id":"pay-bills"}`)
		var run int64
		runs, _ := f.e.DB.SkillRuns(ctx, "pay-bills", "pip", 1)
		run = runs[0].ID
		got := f.call(tr, "mail__send", forwardNewest)
		if i <= skillFloorRuns {
			if !strings.Contains(out, "asks the user first") || !strings.HasPrefix(got, "PENDING") || !runs[0].ForcedAsk {
				t.Fatalf("run %d: %q / %q / forced=%v", i, out, got, runs[0].ForcedAsk)
			}
			if want := "run " + strconv.Itoa(i) + " of 3"; !strings.Contains(tr.afterReply[0].text, want) {
				t.Fatalf("run %d: prompt %q lacks %q", i, tr.afterReply[0].text, want)
			}
		} else if !strings.HasPrefix(got, "sent") {
			t.Fatalf("run %d still floored: %q", i, got)
		}
		f.call(tr, "finish_skill", `{"run":`+strconv.FormatInt(run, 10)+`,"outcome":"succeeded"}`)
		// Once finished, the chat isn't floored any more.
		if _, ok := f.e.floorFor(ctx, f.turn("x"), f.tools["mail__send"]); ok {
			t.Fatalf("run %d: floor outlived the run", i)
		}
	}
	tr := f.turn("x")
	f.call(tr, "use_skill", `{"id":"shared-one"}`)
	if got := f.call(tr, "mail__send", forwardNewest); !strings.HasPrefix(got, "sent") {
		t.Fatalf("a shared skill's run was floored: %q", got)
	}
}

// A floor set after a call was approved still applies when it runs.
func TestApprovedCallMeetsLaterFloor(t *testing.T) {
	ctx := context.Background()
	f := newTaintFixture(t)
	tr := f.turn("forward the newest email to x@y.example")
	f.e.raiseFloor("pip", tr.chatRef, "mcp:mail", Floor{Level: store.LevelRead, Reason: "the account is locked"})
	lv := f.e.toolLevel(ctx, f.char, f.tools["mail__send"])
	if fl, ok := f.e.floorFor(ctx, tr, f.tools["mail__send"]); !ok || stricter(lv, fl.Level) != store.LevelRead {
		t.Fatalf("level %s, floor %+v %v", lv, fl, ok)
	}
}

// A floor kept because it's stricter keeps its own, later expiry.
func TestRaiseFloorNeverShortens(t *testing.T) {
	f := newTaintFixture(t)
	now := time.Now()
	f.e.Now = func() time.Time { return now }
	f.e.raiseFloor("pip", "c", "mcp:mail", Floor{Level: store.LevelRead, Reason: "locked"})
	now = now.Add(-time.Minute) // a writer whose clock read was earlier
	f.e.raiseFloor("pip", "c", "mcp:mail", Floor{Level: store.LevelAsk, Reason: "pay"})
	v, _ := f.e.floors.Load(floorKey{"pip", "c", "mcp:mail"})
	if e := v.(*floorEntry); e.Level != store.LevelRead || e.until.Before(now.Add(time.Minute+floorTTL)) {
		t.Fatalf("floor shortened or lowered: %+v", e)
	}
}
