package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// MemoryKey is one layer of memory: whose, which sprig's ("" = shared),
// and whether it's family memory.
type MemoryKey struct {
	UserID    int64
	Character string
	Household bool
}

// MemoryCount is how many memories a layer holds, and how many of them
// the person set (locked).
type MemoryCount struct {
	Total  int
	Locked int
}

// MemoryCensus counts every memory row, live or expired, so two databases
// (one before a migration, one after) can be compared.
type MemoryCensus struct {
	Layers map[MemoryKey]MemoryCount
	// OrphanUsers are memories whose person no longer exists (the foreign
	// key should make this impossible).
	OrphanUsers int
	// Unlocked are memories a person set (source user) that aren't locked
	// any more: automatic review could change them.
	Unlocked int
	// Users names each person, for messages.
	Users map[int64]string
}

// Total is every memory counted.
func (c *MemoryCensus) Total() int {
	n := 0
	for _, v := range c.Layers {
		n += v.Total
	}
	return n
}

// Keys returns the layers in a stable order.
func (c *MemoryCensus) Keys() []MemoryKey {
	keys := make([]MemoryKey, 0, len(c.Layers))
	for k := range c.Layers {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.UserID != b.UserID {
			return a.UserID < b.UserID
		}
		if a.Character != b.Character {
			return a.Character < b.Character
		}
		return !a.Household && b.Household
	})
	return keys
}

// MemoryCensus counts the memories in the database. It reads only
// columns every schema version has (household from v15), so it works on a
// pre-migration snapshot too.
func (s *Store) MemoryCensus(ctx context.Context) (*MemoryCensus, error) {
	cols, err := s.columns(ctx, "memories")
	if err != nil {
		return nil, err
	}
	household := "0"
	if cols["household"] {
		household = "household"
	}
	c := &MemoryCensus{Layers: map[MemoryKey]MemoryCount{}, Users: map[int64]string{}}
	rows, err := s.db.QueryContext(ctx, "SELECT user_id, character, "+household+", COUNT(*), SUM(locked) FROM memories GROUP BY 1, 2, 3")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k MemoryKey
		var h int
		var n MemoryCount
		if err := rows.Scan(&k.UserID, &k.Character, &h, &n.Total, &n.Locked); err != nil {
			return nil, err
		}
		k.Household = h == 1
		c.Layers[k] = n
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM memories WHERE user_id NOT IN (SELECT id FROM users)),
		(SELECT COUNT(*) FROM memories WHERE source = 'user' AND locked = 0)`).Scan(&c.OrphanUsers, &c.Unlocked); err != nil {
		return nil, err
	}
	urows, err := s.db.QueryContext(ctx, "SELECT id, name FROM users")
	if err != nil {
		return nil, err
	}
	defer urows.Close()
	for urows.Next() {
		var id int64
		var name string
		if err := urows.Scan(&id, &name); err != nil {
			return nil, err
		}
		c.Users[id] = name
	}
	return c, urows.Err()
}

func (s *Store) columns(ctx context.Context, table string) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out[n] = true
	}
	return out, rows.Err()
}

// UsedSince reports whether Thicket did anything with this data after t
// (unix seconds): a message, an activity entry or a rolled-back memory
// review. Doctor compares memory with the pre-migration snapshot only
// while nothing has, since people forget memories in ordinary use.
func (s *Store) UsedSince(ctx context.Context, t int64) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM messages WHERE created_at > ?) +
		(SELECT COUNT(*) FROM activity WHERE at > ?) +
		(SELECT COUNT(*) FROM extraction_runs WHERE rolled_back_at > ?)`, t, t, t).Scan(&n)
	return n > 0, err
}

// OpenSnapshot opens a database file read-only and without touching it
// (no journal, no locks): for reading a pre-migration snapshot.
func OpenSnapshot(ctx context.Context, path string) (*Store, error) {
	u := url.URL{Scheme: "file", Opaque: (&url.URL{Path: path}).EscapedPath(), RawQuery: "mode=ro&immutable=1"}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, path: path}
	if err := s.Ping(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// MemoryLoss compares a census taken before a change with one taken
// after and describes every layer that lost memories or locked memories.
// name labels a sprig id. Layers that grew are fine.
func MemoryLoss(before, after *MemoryCensus, name func(id string) string) []string {
	var out []string
	for _, k := range before.Keys() {
		b, a := before.Layers[k], after.Layers[k]
		who := before.Users[k.UserID]
		if who == "" {
			who = fmt.Sprintf("person #%d", k.UserID)
		}
		layer := who + "'s shared memories"
		switch {
		case k.Household:
			layer = "family memory from " + who
			if k.Character != "" {
				layer = name(k.Character) + "'s family notes from " + who
			}
		case k.Character != "":
			layer = name(k.Character) + "'s memories of " + who
		}
		switch {
		case a.Total < b.Total:
			out = append(out, fmt.Sprintf("%s: %d of %d missing", layer, b.Total-a.Total, b.Total))
		case a.Locked < b.Locked:
			out = append(out, fmt.Sprintf("%s: %d of %d memories they set are no longer locked", layer, b.Locked-a.Locked, b.Locked))
		}
	}
	return out
}

// SprigsWithMemories lists the sprig ids memories point at, with counts.
func (c *MemoryCensus) SprigsWithMemories() map[string]int {
	out := map[string]int{}
	for k, v := range c.Layers {
		if k.Character != "" {
			out[k.Character] += v.Total
		}
	}
	return out
}

// Summary is a one-line description: totals per person and sprig.
func (c *MemoryCensus) Summary(name func(id string) string) string {
	people := map[int64]int{}
	for k, v := range c.Layers {
		people[k.UserID] += v.Total
	}
	ids := make([]int64, 0, len(people))
	for id := range people {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var parts []string
	for _, id := range ids {
		who := c.Users[id]
		if who == "" {
			who = fmt.Sprintf("#%d", id)
		}
		parts = append(parts, fmt.Sprintf("%s %d", who, people[id]))
	}
	sprigs := c.SprigsWithMemories()
	sids := make([]string, 0, len(sprigs))
	for id := range sprigs {
		sids = append(sids, id)
	}
	sort.Strings(sids)
	var sp []string
	for _, id := range sids {
		sp = append(sp, fmt.Sprintf("%s %d", name(id), sprigs[id]))
	}
	out := fmt.Sprintf("%d memories", c.Total())
	if len(parts) > 0 {
		out += "; by person: " + strings.Join(parts, ", ")
	}
	if len(sp) > 0 {
		out += "; by sprig: " + strings.Join(sp, ", ")
	}
	return out
}
