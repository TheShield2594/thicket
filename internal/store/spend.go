package store

import (
	"context"
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
