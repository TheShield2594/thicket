package store

import (
	"context"
	"database/sql"
	"time"
)

// Estimate is what Thicket expected an action to cost before it ran, and
// (once it finished) what it cost.
type Estimate struct {
	ID          int64
	At          time.Time
	UserID      int64
	Character   string
	Kind        string // turn | pdf | task | search
	What        string
	EstimateUSD float64
	Outcome     string // ran | asked | refused
	ActualUSD   float64
	Finished    bool
}

// Estimate outcomes.
const (
	EstimateRan     = "ran"
	EstimateAsked   = "asked"
	EstimateRefused = "refused"
)

// AddEstimate records an estimate and returns its id.
func (s *Store) AddEstimate(ctx context.Context, e Estimate) (int64, error) {
	if e.Outcome == "" {
		e.Outcome = EstimateRan
	}
	res, err := s.db.ExecContext(ctx, "INSERT INTO estimates (at, user_id, character, kind, what, estimate_usd, outcome) VALUES (?, ?, ?, ?, ?, ?, ?)",
		s.unix(), nullInt(e.UserID), e.Character, e.Kind, s.clean(e.What), e.EstimateUSD, e.Outcome)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// SetEstimateOutcome changes what came of an estimate (an action that was
// asked about and then run).
func (s *Store) SetEstimateOutcome(ctx context.Context, id int64, outcome string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE estimates SET outcome = ? WHERE id = ?", outcome, id)
	return err
}

// FinishEstimate records what the estimated action really cost.
func (s *Store) FinishEstimate(ctx context.Context, id int64, actual float64) error {
	_, err := s.db.ExecContext(ctx, "UPDATE estimates SET actual_usd = ?, finished_at = ? WHERE id = ?", actual, s.unix(), id)
	return err
}

// Estimates returns the newest estimates since t.
func (s *Store) Estimates(ctx context.Context, since time.Time, limit int) ([]Estimate, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, at, user_id, character, kind, what, estimate_usd, outcome, actual_usd
		FROM estimates WHERE at >= ? ORDER BY id DESC LIMIT ?`, since.Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Estimate
	for rows.Next() {
		var e Estimate
		var at int64
		var uid sql.NullInt64
		var actual sql.NullFloat64
		if err := rows.Scan(&e.ID, &at, &uid, &e.Character, &e.Kind, &e.What, &e.EstimateUSD, &e.Outcome, &actual); err != nil {
			return nil, err
		}
		e.At, e.UserID, e.ActualUSD, e.Finished = time.Unix(at, 0), uid.Int64, actual.Float64, actual.Valid
		out = append(out, e)
	}
	return out, rows.Err()
}

// EstimateAccuracy sums finished estimates since t: how many, what they
// expected and what they cost.
func (s *Store) EstimateAccuracy(ctx context.Context, since time.Time) (n int, estimated, actual float64, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(estimate_usd), 0), COALESCE(SUM(actual_usd), 0)
		FROM estimates WHERE at >= ? AND actual_usd IS NOT NULL`, since.Unix()).Scan(&n, &estimated, &actual)
	return
}

// ModelRate is what one token has cost on a model since t, averaged over
// prompt and reply tokens (paid calls only). ok is false with too little
// to go on.
func (s *Store) ModelRate(ctx context.Context, model string, since time.Time) (usdPerToken float64, ok bool, err error) {
	var tokens int64
	var cost float64
	err = s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(prompt_tokens + completion_tokens), 0), COALESCE(SUM(cost_usd), 0)
		FROM usage WHERE at >= ? AND model = ? AND cost_usd > 0`, since.Unix(), model).Scan(&tokens, &cost)
	if err != nil || tokens < 1000 {
		return 0, false, err
	}
	return cost / float64(tokens), true, nil
}
