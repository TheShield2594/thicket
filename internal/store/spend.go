package store

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// DaySpend is one UTC day's model spend.
type DaySpend struct {
	Day     time.Time
	CostUSD float64
}

// SpendByDay returns spend per UTC day since t, oldest first.
func (s *Store) SpendByDay(ctx context.Context, since time.Time) ([]DaySpend, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT at / 86400, SUM(cost_usd) FROM usage WHERE at >= ? GROUP BY 1 ORDER BY 1", since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DaySpend
	for rows.Next() {
		var d int64
		var c float64
		if err := rows.Scan(&d, &c); err != nil {
			return nil, err
		}
		out = append(out, DaySpend{Day: time.Unix(d*86400, 0).UTC(), CostUSD: c})
	}
	return out, rows.Err()
}

// UsageRow is model usage grouped by some columns.
type UsageRow struct {
	Keys             []string
	Calls            int
	PromptTokens     int
	CompletionTokens int
	CostUSD          float64
}

var usageGroups = map[string]string{
	"character": "character",
	"origin":    "origin",
	"model":     "model",
	"purpose":   "purpose",
	"day":       "strftime('%Y-%m-%d', at, 'unixepoch')",
}

// UsageSummary groups model usage since t by any of character, origin,
// model, purpose and day (UTC), most expensive first (by day: oldest
// first).
func (s *Store) UsageSummary(ctx context.Context, since time.Time, by ...string) ([]UsageRow, error) {
	cols := make([]string, 0, len(by))
	for _, b := range by {
		c, ok := usageGroups[b]
		if !ok {
			return nil, fmt.Errorf("can't group usage by %q", b)
		}
		cols = append(cols, c)
	}
	sel, group, order := "", "", "SUM(cost_usd) DESC"
	if len(cols) > 0 {
		sel = strings.Join(cols, ", ") + ", "
		nums := make([]string, len(cols))
		for i := range cols {
			nums[i] = strconv.Itoa(i + 1)
		}
		group = " GROUP BY " + strings.Join(nums, ", ")
		if len(by) == 1 && by[0] == "day" {
			order = "1"
		}
	}
	q := "SELECT " + sel + "COUNT(*), COALESCE(SUM(prompt_tokens), 0), COALESCE(SUM(completion_tokens), 0), COALESCE(SUM(cost_usd), 0) FROM usage WHERE at >= ?" + group + " ORDER BY " + order
	rows, err := s.db.QueryContext(ctx, q, since.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageRow
	for rows.Next() {
		r := UsageRow{Keys: make([]string, len(cols))}
		dest := make([]any, 0, len(cols)+4)
		for i := range r.Keys {
			dest = append(dest, &r.Keys[i])
		}
		dest = append(dest, &r.Calls, &r.PromptTokens, &r.CompletionTokens, &r.CostUSD)
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
