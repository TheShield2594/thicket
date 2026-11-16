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

// PromptMix is how chat spend splits between the conversation history
// and kept documents re-sent with each reply, over measured calls.
type PromptMix struct {
	Calls         int     // measured chat calls
	CostUSD       float64 // their cost
	PromptTokens  int
	HistoryTokens int // estimated: prompt tokens × the history's share of the prompt text
	DocTokens     int
	HistoryUSD    float64 // estimated, see OutputPriceRatio
	DocUSD        float64
}

// OutputPriceRatio is how much more an output token is assumed to cost
// than an input token when splitting a call's cost between its prompt
// and its reply (most models charge 3-5 times more for output). The
// provider reports only the total.
const OutputPriceRatio = 4

// HistoryShare is the estimated share of chat spend that was history.
func (m PromptMix) HistoryShare() float64 {
	if m.CostUSD <= 0 {
		return 0
	}
	return m.HistoryUSD / m.CostUSD
}

// DocShare is the estimated share of chat spend that was kept documents.
func (m PromptMix) DocShare() float64 {
	if m.CostUSD <= 0 {
		return 0
	}
	return m.DocUSD / m.CostUSD
}

// PromptMixSince sums the prompt make-up of chat calls since t.
func (s *Store) PromptMixSince(ctx context.Context, since time.Time) (PromptMix, error) {
	var m PromptMix
	input := fmt.Sprintf("(CAST(prompt_tokens AS REAL) / MAX(prompt_tokens + %d * completion_tokens, 1))", OutputPriceRatio)
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(cost_usd), 0), COALESCE(SUM(prompt_tokens), 0),
		COALESCE(SUM(prompt_tokens * history_chars / prompt_chars), 0), COALESCE(SUM(prompt_tokens * document_chars / prompt_chars), 0),
		COALESCE(SUM(cost_usd * `+input+` * history_chars / prompt_chars), 0),
		COALESCE(SUM(cost_usd * `+input+` * document_chars / prompt_chars), 0)
		FROM usage WHERE at >= ? AND purpose = 'chat' AND prompt_chars > 0`, since.Unix()).
		Scan(&m.Calls, &m.CostUSD, &m.PromptTokens, &m.HistoryTokens, &m.DocTokens, &m.HistoryUSD, &m.DocUSD)
	return m, err
}
