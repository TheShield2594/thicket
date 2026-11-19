package store

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
)

// seedMemories writes memories of every kind and layer straight into a
// database at an older schema: shared, a sprig's, family memory (once
// the schema has it), locked, expiring and due ones.
func seedMemories(t *testing.T, s *Store, version int) {
	t.Helper()
	ctx := context.Background()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("schema v%d: %v", version, err)
		}
	}
	exec("INSERT INTO users (id, channel, external_id, name, role, created_at) VALUES (1, 'discord', 'sam', 'Sam', 'owner', 1), (2, 'discord', 'kim', 'Kim', 'member', 1)")
	cols, err := s.columns(ctx, "memories")
	if err != nil {
		t.Fatal(err)
	}
	kinds := []string{"fact", "preference", "person", "project", "inside_joke", "shared_history", "followup", "context"}
	n := 0
	for _, user := range []int64{1, 2} {
		for _, char := range []string{"", "pip"} {
			for i, kind := range kinds {
				n++
				locked, source := 0, "auto"
				if i%3 == 0 {
					locked, source = 1, "user"
				}
				var due, exp any
				if kind == "followup" {
					due = 2000000000
				}
				if kind == "context" {
					exp = 1900000000
				}
				exec(`INSERT INTO memories (user_id, character, kind, content, source, locked, due_at, expires_at, created_at, updated_at)
					VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, 1)`, user, char, kind, fmt.Sprintf("memory %d (%s)", n, kind), source, locked, due, exp)
			}
		}
	}
	if cols["household"] {
		exec(`INSERT INTO memories (user_id, character, kind, content, source, locked, created_at, updated_at, household)
			VALUES (1, '', 'fact', 'The household is vegetarian.', 'auto', 0, 1, 1, 1), (2, '', 'fact', 'Bins go out Thursday.', 'user', 1, 1, 1, 1)`)
	}
}

type memRow struct {
	User                     int64
	Char, Kind, Content, Src string
	Locked, Household        int
	Due, Exp                 any
}

func memRows(t *testing.T, s *Store) map[int64]memRow {
	t.Helper()
	cols, _ := s.columns(context.Background(), "memories")
	household := "0"
	if cols["household"] {
		household = "household"
	}
	rows, err := s.db.Query("SELECT id, user_id, character, kind, content, source, locked, " + household + ", due_at, expires_at FROM memories")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[int64]memRow{}
	for rows.Next() {
		var id int64
		var r memRow
		if err := rows.Scan(&id, &r.User, &r.Char, &r.Kind, &r.Content, &r.Src, &r.Locked, &r.Household, &r.Due, &r.Exp); err != nil {
			t.Fatal(err)
		}
		out[id] = r
	}
	return out
}

// Every migration keeps every memory exactly: a database with memories of
// every kind, made at each earlier schema version, comes through the
// migrations to the current one with the same rows and the same census.
func TestMemorySurvivesEveryMigration(t *testing.T) {
	known, err := KnownMigrations()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for v := 1; v < known; v++ {
		t.Run(fmt.Sprintf("from_v%d", v), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "thicket.db")
			old, err := OpenAtSchema(ctx, path, v)
			if err != nil {
				t.Fatal(err)
			}
			seedMemories(t, old, v)
			before, err := old.MemoryCensus(ctx)
			if err != nil {
				t.Fatal(err)
			}
			rowsBefore := memRows(t, old)
			old.Close()

			s, err := Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			after, err := s.MemoryCensus(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if loss := MemoryLoss(before, after, func(id string) string { return id }); len(loss) > 0 {
				t.Fatalf("lost: %v", loss)
			}
			if !reflect.DeepEqual(before.Layers, after.Layers) || after.Unlocked != 0 || after.OrphanUsers != 0 {
				t.Fatalf("census changed:\nbefore %+v\nafter  %+v", before, after)
			}
			if rowsAfter := memRows(t, s); !reflect.DeepEqual(rowsBefore, rowsAfter) {
				t.Fatalf("rows changed:\nbefore %+v\nafter  %+v", rowsBefore, rowsAfter)
			}
		})
	}
}
