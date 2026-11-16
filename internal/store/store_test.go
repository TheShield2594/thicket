package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func open(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "thicket.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func TestMigrationsAreIdempotent(t *testing.T) {
	ctx := context.Background()
	s, path := open(t)
	v1, _ := s.SchemaVersion(ctx)
	migs, _ := loadMigrations()
	if v1 != len(migs) {
		t.Fatalf("schema %d, want %d", v1, len(migs))
	}
	s.Close()
	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if v2, _ := s2.SchemaVersion(ctx); v2 != v1 {
		t.Fatalf("reopen changed version %d -> %d", v1, v2)
	}
}

func TestRefusesNewerSchema(t *testing.T) {
	ctx := context.Background()
	s, path := open(t)
	if _, err := s.db.ExecContext(ctx, "INSERT INTO schema_migrations (version, name, applied_at) VALUES (999, 'future', 0)"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := Open(ctx, path); err == nil || !strings.Contains(err.Error(), "newer database") {
		t.Fatalf("want refusal, got %v", err)
	}
}

func TestMemoryChangesAndRollback(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	u, _ := s.CreateUser(ctx, "discord", "1", "Sam", "owner")
	ids, err := s.ApplyMemoryChanges(ctx, 0, []MemoryChange{{Action: "add", Memory: Memory{UserID: u.ID, Kind: "fact", Content: "Likes tea.", Source: "auto"}}})
	if err != nil {
		t.Fatal(err)
	}
	keep := ids[0]
	run, _ := s.CreateExtractionRun(ctx, 0, u.ID, "m")
	ids, err = s.ApplyMemoryChanges(ctx, run, []MemoryChange{
		{Action: "add", Memory: Memory{UserID: u.ID, Character: "juniper", Kind: "inside_joke", Content: "Gerald the plant.", Source: "auto"}},
		{Action: "update", Memory: Memory{ID: keep, Content: "Likes green tea."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if mems, _ := s.VisibleMemories(ctx, u.ID, "juniper"); len(mems) != 2 {
		t.Fatalf("juniper should see 2, got %d", len(mems))
	}
	if mems, _ := s.VisibleMemories(ctx, u.ID, "pip"); len(mems) != 1 {
		t.Fatalf("pip should only see shared, got %d", len(mems))
	}
	n, err := s.RollbackRun(ctx, run)
	if err != nil || n != 2 {
		t.Fatalf("rollback: %d %v", n, err)
	}
	mems, _ := s.AllMemories(ctx, u.ID)
	if len(mems) != 1 || mems[0].Content != "Likes tea." {
		t.Fatalf("rollback did not restore: %+v", mems)
	}
	if _, err := s.RollbackRun(ctx, run); err == nil {
		t.Fatal("double rollback should fail")
	}
}

func TestForgetScrubsHistory(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	u, _ := s.CreateUser(ctx, "discord", "1", "", "owner")
	ids, _ := s.ApplyMemoryChanges(ctx, 0, []MemoryChange{{Action: "add", Memory: Memory{UserID: u.ID, Kind: "fact", Content: "Secret crush on Alex.", Source: "auto"}}})
	if _, err := s.ApplyMemoryChanges(ctx, 0, []MemoryChange{{Action: "delete", Memory: Memory{ID: ids[0]}}}); err != nil {
		t.Fatal(err)
	}
	var n int
	s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM memory_events WHERE memory_id = ?", ids[0]).Scan(&n)
	if n != 0 {
		t.Fatalf("forgotten memory left %d history rows", n)
	}
}

func TestExpiryAndSweep(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return now })
	u, _ := s.CreateUser(ctx, "discord", "1", "", "owner")
	s.ApplyMemoryChanges(ctx, 0, []MemoryChange{{Action: "add", Memory: Memory{UserID: u.ID, Kind: "context", Content: "Tired today.", Source: "auto", ExpiresAt: now.Add(time.Hour)}}})
	if m, _ := s.AllMemories(ctx, u.ID); len(m) != 1 {
		t.Fatal("not yet expired")
	}
	now = now.Add(2 * time.Hour)
	if m, _ := s.AllMemories(ctx, u.ID); len(m) != 0 {
		t.Fatal("expired memory still visible")
	}
	if n, _ := s.SweepMemories(ctx, 24*time.Hour); n != 0 {
		t.Fatal("swept inside grace window")
	}
	now = now.Add(48 * time.Hour)
	if n, _ := s.SweepMemories(ctx, 24*time.Hour); n != 1 {
		t.Fatal("expired memory not swept")
	}
}

func TestConversationsGoIdle(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return now })
	u, _ := s.CreateUser(ctx, "discord", "1", "", "owner")
	c1, _ := s.OpenConversation(ctx, u.ID, "discord:dm", 20*time.Minute)
	s.AddMessage(ctx, c1.ID, "user", "", "hi")
	now = now.Add(10 * time.Minute)
	if c, _ := s.OpenConversation(ctx, u.ID, "discord:dm", 20*time.Minute); c.ID != c1.ID {
		t.Fatal("conversation should continue")
	}
	if todo, _ := s.ConversationsToReview(ctx, 20*time.Minute, 10); len(todo) != 0 {
		t.Fatal("not idle yet")
	}
	now = now.Add(30 * time.Minute)
	if todo, _ := s.ConversationsToReview(ctx, 20*time.Minute, 10); len(todo) != 1 {
		t.Fatal("idle conversation should be up for review")
	}
	if c, _ := s.OpenConversation(ctx, u.ID, "discord:dm", 20*time.Minute); c.ID == c1.ID {
		t.Fatal("idle conversation should not be reused")
	}
}

func TestClaimNotice(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	now := time.Unix(1_800_000_000, 0)
	s.SetClock(func() time.Time { return now })
	if ok, _ := s.ClaimNotice(ctx, "k", time.Hour); !ok {
		t.Fatal("first claim")
	}
	if ok, _ := s.ClaimNotice(ctx, "k", time.Hour); ok {
		t.Fatal("duplicate within window")
	}
	now = now.Add(2 * time.Hour)
	if ok, _ := s.ClaimNotice(ctx, "k", time.Hour); !ok {
		t.Fatal("claim after window")
	}
}

func TestApprovals(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	u, _ := s.CreateUser(ctx, "discord", "1", "", "owner")
	id, _ := s.CreateApproval(ctx, Approval{UserID: u.ID, Character: "juniper", ChatRef: "discord:1", Tool: "mcp_gmail_send", Args: "{}"})
	a, err := s.LatestPendingApproval(ctx, u.ID, "discord:1", time.Hour)
	if err != nil || a.ID != id {
		t.Fatalf("got %+v %v", a, err)
	}
	if ok, _ := s.DecideApproval(ctx, id, "approved"); !ok {
		t.Fatal("decide")
	}
	if ok, _ := s.DecideApproval(ctx, id, "denied"); ok {
		t.Fatal("double decision must fail")
	}
}

func TestSnapshot(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	dest := filepath.Join(t.TempDir(), "snap.db")
	if err := s.SnapshotTo(ctx, dest); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(dest); err != nil || fi.Size() == 0 {
		t.Fatalf("snapshot missing: %v", err)
	}
}

func TestRetentionCoversAbandonedConversations(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return now })
	u, _ := s.CreateUser(ctx, "discord", "1", "", "owner")
	c, _ := s.OpenConversation(ctx, u.ID, "discord:dm", time.Minute)
	s.AddMessage(ctx, c.ID, "user", "", "private words")
	for i := 0; i < 3; i++ {
		s.MarkReviewFailed(ctx, c.ID)
	}
	now = now.Add(40 * 24 * time.Hour)
	if n, err := s.PruneMessages(ctx, 30*24*time.Hour); err != nil || n != 1 {
		t.Fatalf("abandoned conversation not pruned: %d %v", n, err)
	}
}

func TestSweepRemovesHistory(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return now })
	u, _ := s.CreateUser(ctx, "discord", "1", "", "owner")
	ids, _ := s.ApplyMemoryChanges(ctx, 0, []MemoryChange{{Action: "add", Memory: Memory{UserID: u.ID, Kind: "context", Content: "Tired today.", Source: "auto", ExpiresAt: now.Add(time.Hour)}}})
	now = now.Add(60 * 24 * time.Hour)
	if n, _ := s.SweepMemories(ctx, 30*24*time.Hour); n != 1 {
		t.Fatal("not swept")
	}
	var events int
	s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM memory_events WHERE memory_id = ?", ids[0]).Scan(&events)
	if events != 0 {
		t.Fatalf("%d history rows left after sweep", events)
	}
}

func TestAutomaticReviewRespectsLateLock(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	u, _ := s.CreateUser(ctx, "discord", "1", "", "owner")
	ids, _ := s.ApplyMemoryChanges(ctx, 0, []MemoryChange{{Action: "add", Memory: Memory{UserID: u.ID, Kind: "fact", Content: "Likes tea.", Source: "auto"}}})
	s.LockMemory(ctx, ids[0]) // user locks it while a review is in flight
	run, _ := s.CreateExtractionRun(ctx, 0, u.ID, "m")
	if _, err := s.ApplyMemoryChanges(ctx, run, []MemoryChange{
		{Action: "update", Memory: Memory{ID: ids[0], Content: "Likes coffee."}},
		{Action: "expire", Memory: Memory{ID: ids[0]}},
	}); err != nil {
		t.Fatal(err)
	}
	m, _ := s.MemoryByID(ctx, ids[0])
	if m.Content != "Likes tea." || !m.ExpiresAt.IsZero() {
		t.Fatalf("locked memory changed by review: %+v", m)
	}
}

func TestMarkReviewedSkipsNewActivity(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return now })
	u, _ := s.CreateUser(ctx, "discord", "1", "", "owner")
	c, _ := s.OpenConversation(ctx, u.ID, "discord:dm", time.Hour)
	s.AddMessage(ctx, c.ID, "user", "", "hi")
	snapshot := now
	now = now.Add(time.Minute)
	s.AddMessage(ctx, c.ID, "user", "", "one more thing") // arrives during review
	if ok, _ := s.MarkReviewed(ctx, c.ID, snapshot); ok {
		t.Fatal("conversation with new messages was closed")
	}
	if ok, _ := s.MarkReviewed(ctx, c.ID, now); !ok {
		t.Fatal("unchanged conversation should close")
	}
}

func TestClaimOnceNeverExpires(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	now := time.Unix(1_800_000_000, 0)
	s.SetClock(func() time.Time { return now })
	if ok, _ := s.ClaimOnce(ctx, "checkin:1:123"); !ok {
		t.Fatal("first claim")
	}
	now = now.Add(20 * 365 * 24 * time.Hour) // a very long quiet spell
	if ok, _ := s.ClaimOnce(ctx, "checkin:1:123"); ok {
		t.Fatal("same quiet spell claimed twice")
	}
	if ok, _ := s.ClaimOnce(ctx, "checkin:1:456"); !ok {
		t.Fatal("a new quiet spell should be claimable")
	}
}

func TestRollbackOfAddLeavesNoContent(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	u, _ := s.CreateUser(ctx, "discord", "1", "", "owner")
	run, _ := s.CreateExtractionRun(ctx, 0, u.ID, "m")
	ids, _ := s.ApplyMemoryChanges(ctx, run, []MemoryChange{{Action: "add", Memory: Memory{UserID: u.ID, Kind: "fact", Content: "Private detail.", Source: "auto"}}})
	// A later user-initiated edit (run_id NULL) of the same memory.
	s.ApplyMemoryChanges(ctx, 0, []MemoryChange{{Action: "update", Memory: Memory{ID: ids[0], Content: "Private detail, edited."}}})
	if _, err := s.RollbackRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	var n int
	s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM memory_events WHERE memory_id = ? AND (before LIKE '%Private%' OR after LIKE '%Private%')", ids[0]).Scan(&n)
	if n != 0 {
		t.Fatalf("%d history rows still hold the rolled-back memory's content", n)
	}
	s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM memory_events WHERE run_id IS NULL AND memory_id = ?", ids[0]).Scan(&n)
	if n != 0 {
		t.Fatal("user-initiated history of a rolled-back memory was kept")
	}
}

// A v1 database with history upgrades in place: messages survive the
// table rebuild, cascades still work, and the new role is accepted.
func TestMigrateV1MessagesToDocumentRole(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "thicket.db")
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	init, _ := migrationFS.ReadFile("migrations/0001_init.sql")
	for _, q := range []string{
		"CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at INTEGER NOT NULL)",
		string(init),
		"INSERT INTO schema_migrations VALUES (1, '0001_init.sql', 0)",
		"INSERT INTO users (id, channel, external_id, created_at) VALUES (1, 'discord', 'u', 0)",
		"INSERT INTO conversations (id, user_id, chat_ref, started_at, last_activity_at) VALUES (7, 1, 'discord:c', 0, 0)",
		"INSERT INTO messages (conversation_id, role, content, created_at) VALUES (7, 'user', 'hello', 0)",
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
	if err := s.AddMessage(ctx, 7, "document", "", "report.pdf\ntext"); err != nil {
		t.Fatalf("document role refused after migration: %v", err)
	}
	msgs, err := s.RecentMessages(ctx, 7, 10)
	if err != nil || len(msgs) != 2 || msgs[0].Content != "hello" || msgs[1].Role != "document" {
		t.Fatalf("messages after migration: %+v %v", msgs, err)
	}
	if err := s.AddMessage(ctx, 7, "robot", "", "x"); err == nil {
		t.Fatal("unknown role accepted")
	}
	if _, err := s.DB().Exec("DELETE FROM conversations WHERE id = 7"); err != nil {
		t.Fatal(err)
	}
	if msgs, _ := s.RecentMessages(ctx, 7, 10); len(msgs) != 0 {
		t.Fatal("messages didn't cascade with their conversation")
	}
}

func TestIdentities(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	owner, _ := s.CreateUser(ctx, "discord", "d1", "Sam", "owner")
	if u, err := s.UserByExternal(ctx, "discord", "d1"); err != nil || u.ID != owner.ID {
		t.Fatalf("lookup by first identity: %+v %v", u, err)
	}

	// A brand-new account links and then resolves to the same person.
	if err := s.LinkIdentity(ctx, owner.ID, "telegram", "t1"); err != nil {
		t.Fatal(err)
	}
	if u, err := s.UserByExternal(ctx, "telegram", "t1"); err != nil || u.ID != owner.ID {
		t.Fatalf("lookup by linked identity: %+v %v", u, err)
	}
	if err := s.LinkIdentity(ctx, owner.ID, "telegram", "t1"); err != nil {
		t.Fatalf("relinking the same account: %v", err)
	}

	// Writing from Telegram makes it where proactive messages go.
	if err := s.UseIdentity(ctx, owner.ID, "telegram", "t1"); err != nil {
		t.Fatal(err)
	}
	if u, _ := s.UserByID(ctx, owner.ID); u.Channel != "telegram" || u.ExternalID != "t1" {
		t.Fatalf("primary identity: %+v", u)
	}

	// An empty profile on the other platform is folded in...
	empty, _ := s.CreateUser(ctx, "telegram", "t2", "", "member")
	if err := s.LinkIdentity(ctx, owner.ID, "telegram", "t2"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UserByID(ctx, empty.ID); err != ErrNotFound {
		t.Fatal("the empty profile should be gone")
	}
	// ...but one with a history, or an owner, is never taken over.
	busy, _ := s.CreateUser(ctx, "telegram", "t3", "", "member")
	if _, err := s.DB().Exec("INSERT INTO memories (user_id, kind, content, source, created_at, updated_at) VALUES (?, 'fact', 'x', 'auto', 0, 0)", busy.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.LinkIdentity(ctx, owner.ID, "telegram", "t3"); err != ErrIdentityTaken {
		t.Fatalf("linking an account with memories: %v", err)
	}
	member, _ := s.CreateUser(ctx, "discord", "d9", "", "member")
	if err := s.LinkIdentity(ctx, member.ID, "discord", "d1"); err != ErrIdentityTaken {
		t.Fatalf("taking the owner's account: %v", err)
	}
	if ids, _ := s.Identities(ctx, owner.ID); len(ids) != 3 {
		t.Fatalf("identities: %+v", ids)
	}
}
