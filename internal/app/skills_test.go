package app

import (
	"strings"
	"testing"

	"github.com/theshield2594/thicket/internal/channel"
	"github.com/theshield2594/thicket/internal/llm"
)

// lastToolResult is the newest tool message the model was given.
func lastToolResult(fm *fakeModel) string {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	for i := len(fm.reqs) - 1; i >= 0; i-- {
		msgs := fm.reqs[i].Messages
		for j := len(msgs) - 1; j >= 0; j-- {
			if msgs[j].Role == "tool" {
				return msgs[j].Content
			}
		}
	}
	return ""
}

func systemPromptOf(req llm.Request) string { return req.Messages[0].Content }

func TestSkills(t *testing.T) {
	a, fc, fm, ctx := setup(t)
	e := a.Engine
	onboard(t, a, fc, ctx)

	// Admins are told they can save skills even before any exist.
	e.HandleIncoming(ctx, dm("hello"))
	fc.take()
	if p := systemPromptOf(lastChatReq(fm)); !strings.Contains(p, "offer to save it as a skill") {
		t.Fatal("admin prompt doesn't mention saving skills")
	}

	e.HandleIncoming(ctx, dm("save skill with key"))
	fc.take()
	if r := lastToolResult(fm); !strings.Contains(r, "can't contain keys") {
		t.Fatalf("secret in a skill: %s", r)
	}
	e.HandleIncoming(ctx, dm("save skill briefing"))
	fc.take()
	if r := lastToolResult(fm); !strings.Contains(r, "created: version 1") {
		t.Fatalf("create: %s", r)
	}

	// Every sprig now sees it in the index, and reads it before using it.
	e.HandleIncoming(ctx, dm("use the briefing skill please"))
	fc.take()
	if p := systemPromptOf(lastChatReq(fm)); !strings.Contains(p, "- briefing: Morning briefing. When putting together the morning summary") {
		t.Fatalf("index missing from prompt:\n%s", p)
	}
	if r := lastToolResult(fm); !strings.Contains(r, "version 1") || !strings.Contains(r, "2. Calendar") {
		t.Fatalf("read: %s", r)
	}

	e.HandleIncoming(ctx, dm("save skill v2"))
	fc.take()
	if r := lastToolResult(fm); !strings.Contains(r, "revised: version 2") {
		t.Fatalf("revise: %s", r)
	}
	// An edit based on an old read is refused, not silently applied.
	e.HandleIncoming(ctx, dm("save skill stale"))
	fc.take()
	if r := lastToolResult(fm); !strings.Contains(r, "changed since you read it (now version 2)") {
		t.Fatalf("stale edit: %s", r)
	}
	e.HandleIncoming(ctx, dm("restore briefing to 1"))
	fc.take()
	k, _ := a.DB.SkillByID(ctx, "briefing", 0)
	if k.Version != 3 || strings.Contains(k.Body, "nice thing") || k.Note != "restored version 1" {
		t.Fatalf("restore: %+v", k)
	}

	// Members can read skills but not change them: the tool isn't offered,
	// and calling it anyway is refused in code.
	a.DB.CreateUser(ctx, "discord", "member", "", "member")
	member := func(text string) channel.Incoming {
		in := dm(text)
		in.UserID, in.ChatID = "member", "dm-member"
		return in
	}
	e.HandleIncoming(ctx, member("hi"))
	fc.take()
	req := lastChatReq(fm)
	for _, d := range req.Tools {
		if d.Function.Name == "save_skill" || d.Function.Name == "manage_skill" {
			t.Fatalf("%s offered to a member", d.Function.Name)
		}
	}
	if strings.Contains(systemPromptOf(req), "offer to save it as a skill") {
		t.Fatal("members shouldn't be offered skill saving")
	}
	e.HandleIncoming(ctx, member("save skill v2"))
	fc.take()
	if r := lastToolResult(fm); !strings.Contains(r, "don't have access") {
		t.Fatalf("member save: %s", r)
	}
	if k, _ := a.DB.SkillByID(ctx, "briefing", 0); k.Version != 3 {
		t.Fatal("a member changed a skill")
	}
}
