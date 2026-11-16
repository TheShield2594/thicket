package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Every schema change goes through a numbered migration: no Go file in
// the store creates or alters tables (schema_migrations itself excepted).
func TestSchemaChangesOnlyInMigrations(t *testing.T) {
	ddl := regexp.MustCompile(`(?i)\b(CREATE|ALTER|DROP)\s+(TABLE|INDEX)\b`)
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(f)
		for _, line := range strings.Split(string(b), "\n") {
			if ddl.MatchString(line) && !strings.Contains(line, "schema_migrations") {
				t.Errorf("%s: schema change outside a migration: %s", f, strings.TrimSpace(line))
			}
		}
	}
}

// Migration 10 turns the old global approval rules into levels for every
// sprig.
func TestMigrateApprovalPoliciesToLevels(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "thicket.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	migs, _ := loadMigrations()
	if _, err := db.Exec("CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at INTEGER NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	for _, m := range migs[:9] {
		if _, err := db.Exec(m.sql); err != nil {
			t.Fatal(m.name, err)
		}
		db.Exec("INSERT INTO schema_migrations VALUES (?, ?, 0)", m.version, m.name)
	}
	for _, q := range []string{
		"INSERT INTO tool_policies VALUES ('mcp_ha__turn_on', 'never', 5)",
		"INSERT INTO tool_policies VALUES ('remember', 'always', 5)",
		"INSERT INTO usage (at, purpose, model) VALUES (1, 'job', 'm'), (1, 'chat', 'm'), (1, 'memory', 'm')",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for tool, want := range map[string]string{"mcp_ha__turn_on": LevelAct, "remember": LevelAsk, "other": ""} {
		if got, _ := s.ToolLevel(ctx, "pip", tool, true); got != want {
			t.Errorf("%s: level %q, want %q", tool, got, want)
		}
	}
	rows, _ := s.UsageSummary(ctx, time.Unix(0, 0), "origin")
	got := map[string]int{}
	for _, r := range rows {
		got[r.Keys[0]] = r.Calls
	}
	if got["heartbeat"] != 2 || got["task"] != 1 {
		t.Fatalf("existing usage not tagged: %v", got)
	}
}

func TestToolLevelPrecedence(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	if err := s.SetToolLevel(ctx, AllSprigs, "send", LevelAct); err != nil {
		t.Fatal(err)
	}
	if err := s.SetToolLevel(ctx, "pip", "send", LevelRead); err != nil {
		t.Fatal(err)
	}
	if lv, _ := s.ToolLevel(ctx, "pip", "send", true); lv != LevelRead {
		t.Fatalf("a sprig's own level must win: %q", lv)
	}
	if lv, _ := s.ToolLevel(ctx, "juniper", "send", true); lv != LevelAct {
		t.Fatalf("others get the all-sprigs level: %q", lv)
	}
	if err := s.SetToolLevel(ctx, "pip", "send", "sudo"); err == nil {
		t.Fatal("an unknown level was accepted")
	}
	s.SetToolLevel(ctx, "pip", "send", "")
	if lv, _ := s.ToolLevel(ctx, "pip", "send", true); lv != LevelAct {
		t.Fatalf("clearing the sprig's level falls back: %q", lv)
	}
}

func TestInboxDedupesOpenItems(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	u, _ := s.CreateUser(ctx, "discord", "1", "Sam", "owner")
	other, _ := s.CreateUser(ctx, "discord", "2", "Al", "member")
	id1, _ := s.AddInbox(ctx, InboxItem{UserID: u.ID, Kind: InboxFailed, Ref: "job:4", Title: "Job failed", Detail: "first"})
	id2, _ := s.AddInbox(ctx, InboxItem{UserID: u.ID, Kind: InboxFailed, Ref: "job:4", Title: "Job failed", Detail: "second"})
	if id1 != id2 {
		t.Fatal("the same open failure became two items")
	}
	s.AddInbox(ctx, InboxItem{UserID: u.ID, Kind: InboxDraft, Title: "draft a"})
	s.AddInbox(ctx, InboxItem{UserID: u.ID, Kind: InboxDraft, Title: "draft b"}) // no ref: never merged
	items, _ := s.OpenInbox(ctx, u.ID)
	if len(items) != 3 {
		t.Fatalf("want 3 items, got %+v", items)
	}
	for _, it := range items {
		if it.ID == id1 && (it.Count != 2 || it.Detail != "second") {
			t.Fatalf("repeat not counted: %+v", it)
		}
	}
	if err := s.ResolveInbox(ctx, id1, other.ID); err != ErrNotFound {
		t.Fatalf("another user resolved my item: %v", err)
	}
	if err := s.ResolveInbox(ctx, id1, u.ID); err != nil {
		t.Fatal(err)
	}
	// Once resolved, a new failure is a new item.
	if id3, _ := s.AddInbox(ctx, InboxItem{UserID: u.ID, Kind: InboxFailed, Ref: "job:4", Title: "Job failed"}); id3 == id1 {
		t.Fatal("a resolved item was reopened")
	}
}

func TestStaleRunsAndTasksAreReturned(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	u, _ := s.CreateUser(ctx, "discord", "1", "Sam", "owner")
	j, _ := s.CreateJob(ctx, Job{UserID: u.ID, Character: "juniper", Name: "n", Schedule: "@daily", Prompt: "p"})
	s.StartJobRun(ctx, j.ID, time.Now())
	tid, _ := s.StartTask(ctx, Task{UserID: u.ID, From: "juniper", Character: "pip", Prompt: "look"})
	done, _ := s.StartTask(ctx, Task{UserID: u.ID, From: "juniper", Character: "pip", Prompt: "done already"})
	s.FinishTask(ctx, done, "ok", "")

	runs, err := s.FailStaleRuns(ctx)
	if err != nil || len(runs) != 1 || runs[0].JobID != j.ID {
		t.Fatalf("stale runs: %+v %v", runs, err)
	}
	if again, _ := s.FailStaleRuns(ctx); len(again) != 0 {
		t.Fatal("a stale run was reported twice")
	}
	tasks, err := s.InterruptRunningTasks(ctx)
	if err != nil || len(tasks) != 1 || tasks[0].ID != tid {
		t.Fatalf("interrupted tasks: %+v %v", tasks, err)
	}
	if again, _ := s.InterruptRunningTasks(ctx); len(again) != 0 {
		t.Fatal("an interrupted task was reported twice")
	}
}

func TestUsageSummary(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	for _, u := range []Usage{
		{Character: "juniper", Origin: "task", Model: "a", PromptTokens: 10, CompletionTokens: 5, CostUSD: 0.5},
		{Character: "juniper", Origin: "heartbeat", Model: "a", CostUSD: 0.25},
		{Character: "pip", Origin: "heartbeat", Model: "b", CostUSD: 1},
	} {
		if err := s.RecordUsage(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.UsageSummary(ctx, time.Now().Add(-time.Hour), "character")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Keys[0] != "pip" || rows[1].Keys[0] != "juniper" || rows[1].CostUSD != 0.75 || rows[1].Calls != 2 || rows[1].PromptTokens != 10 {
		t.Fatalf("by character: %+v", rows)
	}
	total, _ := s.UsageSummary(ctx, time.Now().Add(-time.Hour))
	if len(total) != 1 || total[0].Calls != 3 || total[0].CostUSD != 1.75 {
		t.Fatalf("total: %+v", total)
	}
	if _, err := s.UsageSummary(ctx, time.Now(), "user_id; DROP TABLE usage"); err == nil {
		t.Fatal("arbitrary grouping accepted")
	}
}

// A sprig-wide level (tool "*") applies to granted tools only, and loses
// to a level set for that exact tool.
func TestSprigWideLevel(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	s.SetToolLevel(ctx, "pip", AnyTool, LevelDraft)
	s.SetToolLevel(ctx, AllSprigs, "send", LevelAct)
	if lv, _ := s.ToolLevel(ctx, "pip", "send", true); lv != LevelDraft {
		t.Fatalf("sprig-wide level should beat the all-sprigs one: %q", lv)
	}
	if lv, _ := s.ToolLevel(ctx, "pip", "remember", false); lv != "" {
		t.Fatalf("a core tool picked up the sprig-wide level: %q", lv)
	}
	s.SetToolLevel(ctx, "pip", "send", LevelAsk)
	if lv, _ := s.ToolLevel(ctx, "pip", "send", true); lv != LevelAsk {
		t.Fatalf("the exact tool level should win: %q", lv)
	}
	if LevelRank(LevelRead) >= LevelRank(LevelAct) || LevelRank("nope") != -1 {
		t.Fatal("level ranks")
	}
}
