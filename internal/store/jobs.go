package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"
)

type Job struct {
	ID        int64
	UserID    int64
	Character string
	Name      string
	Schedule  string // cron spec; empty for webhook jobs
	Prompt    string
	Kind      string // cron | webhook
	Enabled   bool
	LastRunAt time.Time
	CreatedAt time.Time
	// HookHash is the SHA-256 (hex) of a webhook job's URL token; only
	// read when creating a job.
	HookHash string
	// HookAuth is how a webhook job's calls must be signed: "" (none),
	// "github", "gitea" or "gitlab".
	HookAuth string
	// Payload is what a webhook call sent. Not stored: it lives only for
	// the run it started.
	Payload string
}

const (
	JobCron    = "cron"
	JobWebhook = "webhook"
)

type JobRun struct {
	ID         int64
	JobID      int64
	StartedAt  time.Time
	FinishedAt time.Time
	Status     string // running | ok | empty | error | missed
	Output     string
	Error      string
	CostUSD    float64
}

const jobCols = "id, user_id, character, name, schedule, prompt, enabled, last_run_at, created_at, kind, hook_auth"

func scanJob(r interface{ Scan(...any) error }) (*Job, error) {
	var j Job
	var en int
	var last sql.NullInt64
	var created int64
	if err := r.Scan(&j.ID, &j.UserID, &j.Character, &j.Name, &j.Schedule, &j.Prompt, &en, &last, &created, &j.Kind, &j.HookAuth); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	j.Enabled, j.LastRunAt, j.CreatedAt = en == 1, timeOf(last), time.Unix(created, 0)
	return &j, nil
}

func (s *Store) CreateJob(ctx context.Context, j Job) (*Job, error) {
	if j.Kind == "" {
		j.Kind = JobCron
	}
	var hook any
	if j.HookHash != "" {
		hook = j.HookHash
	}
	res, err := s.db.ExecContext(ctx, "INSERT INTO jobs (user_id, character, name, schedule, prompt, enabled, created_at, kind, hook_hash, hook_auth) VALUES (?, ?, ?, ?, ?, 1, ?, ?, ?, ?)",
		j.UserID, j.Character, j.Name, j.Schedule, j.Prompt, s.unix(), j.Kind, hook, j.HookAuth)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.JobByID(ctx, id)
}

func (s *Store) JobByID(ctx context.Context, id int64) (*Job, error) {
	return scanJob(s.db.QueryRowContext(ctx, "SELECT "+jobCols+" FROM jobs WHERE id = ?", id))
}

// JobByHook finds a webhook job by the hash of its token.
func (s *Store) JobByHook(ctx context.Context, hash string) (*Job, error) {
	return scanJob(s.db.QueryRowContext(ctx, "SELECT "+jobCols+" FROM jobs WHERE kind = 'webhook' AND hook_hash = ?", hash))
}

// SetJobHook replaces a webhook job's token hash (the old URL stops working).
func (s *Store) SetJobHook(ctx context.Context, id int64, hash string) error {
	res, err := s.db.ExecContext(ctx, "UPDATE jobs SET hook_hash = ? WHERE id = ? AND kind = 'webhook'", hash, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrNotFound
	}
	return nil
}

// SetJobHookAuth sets how a webhook job's calls must be signed.
func (s *Store) SetJobHookAuth(ctx context.Context, id int64, auth string) error {
	res, err := s.db.ExecContext(ctx, "UPDATE jobs SET hook_auth = ? WHERE id = ? AND kind = 'webhook'", auth, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrNotFound
	}
	return nil
}

// JobRunsSince counts a job's runs started at or after since.
func (s *Store) JobRunsSince(ctx context.Context, jobID int64, since time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM job_runs WHERE job_id = ? AND started_at >= ? AND status != 'missed'", jobID, since.Unix()).Scan(&n)
	return n, err
}

func (s *Store) listJobs(ctx context.Context, q string, args ...any) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

func (s *Store) EnabledJobs(ctx context.Context) ([]Job, error) {
	return s.listJobs(ctx, "SELECT "+jobCols+" FROM jobs WHERE enabled = 1 ORDER BY id")
}

func (s *Store) UserJobs(ctx context.Context, userID int64) ([]Job, error) {
	return s.listJobs(ctx, "SELECT "+jobCols+" FROM jobs WHERE user_id = ? ORDER BY id", userID)
}

func (s *Store) SetJobEnabled(ctx context.Context, id int64, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	_, err := s.db.ExecContext(ctx, "UPDATE jobs SET enabled = ? WHERE id = ?", v, id)
	return err
}

func (s *Store) DeleteJob(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM jobs WHERE id = ?", id)
	return err
}

// StartJobRun records the start of a run and advances last_run_at so a
// crash mid-run can't cause the job to fire in a tight loop.
func (s *Store) StartJobRun(ctx context.Context, jobID int64, at time.Time) (int64, error) {
	if _, err := s.db.ExecContext(ctx, "UPDATE jobs SET last_run_at = ? WHERE id = ?", at.Unix(), jobID); err != nil {
		return 0, err
	}
	res, err := s.db.ExecContext(ctx, "INSERT INTO job_runs (job_id, started_at, status) VALUES (?, ?, 'running')", jobID, s.unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) FinishJobRun(ctx context.Context, runID int64, status, output, errText string, cost float64) error {
	_, err := s.db.ExecContext(ctx, "UPDATE job_runs SET finished_at = ?, status = ?, output = ?, error = ?, cost_usd = ? WHERE id = ?",
		s.unix(), status, output, errText, cost, runID)
	return err
}

// RecordSkippedRun notes a run that was held back on purpose (spend cap)
// and moves the job's last run time on, so it isn't counted as missed.
func (s *Store) RecordSkippedRun(ctx context.Context, jobID int64, at time.Time, why string) error {
	if _, err := s.db.ExecContext(ctx, "UPDATE jobs SET last_run_at = ? WHERE id = ?", at.Unix(), jobID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, "INSERT INTO job_runs (job_id, started_at, finished_at, status, error) VALUES (?, ?, ?, 'missed', ?)",
		jobID, at.Unix(), at.Unix(), "skipped: "+why)
	return err
}

// RecordMissedRun notes runs that were skipped while Thicket was down, so
// history shows the gap instead of silently hiding it.
func (s *Store) RecordMissedRun(ctx context.Context, jobID int64, count int, at time.Time) error {
	_, err := s.db.ExecContext(ctx, "INSERT INTO job_runs (job_id, started_at, finished_at, status, error) VALUES (?, ?, ?, 'missed', ?)",
		jobID, at.Unix(), at.Unix(), "missed "+strconv.Itoa(count)+" scheduled run(s) while Thicket was offline")
	return err
}

// FailStaleRuns marks runs left 'running' by a stop or crash as errors
// and returns them, so each can be reported.
func (s *Store) FailStaleRuns(ctx context.Context) ([]JobRun, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id, job_id, started_at FROM job_runs WHERE status = 'running'")
	if err != nil {
		return nil, err
	}
	var out []JobRun
	for rows.Next() {
		var r JobRun
		var st int64
		if err := rows.Scan(&r.ID, &r.JobID, &st); err != nil {
			rows.Close()
			return nil, err
		}
		r.StartedAt, r.Status, r.Error = time.Unix(st, 0), "error", StaleRunError
		out = append(out, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE job_runs SET status = 'error', error = ?, finished_at = ? WHERE status = 'running'", StaleRunError, s.unix()); err != nil {
		return nil, err
	}
	return out, nil
}

// StaleRunError is the error recorded for a run Thicket stopped during.
const StaleRunError = "Thicket stopped while this run was in progress"

func (s *Store) JobRuns(ctx context.Context, jobID int64, limit int) ([]JobRun, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id, job_id, started_at, finished_at, status, output, error, cost_usd FROM job_runs WHERE job_id = ? ORDER BY id DESC LIMIT ?", jobID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []JobRun
	for rows.Next() {
		var r JobRun
		var st int64
		var fin sql.NullInt64
		if err := rows.Scan(&r.ID, &r.JobID, &st, &fin, &r.Status, &r.Output, &r.Error, &r.CostUSD); err != nil {
			return nil, err
		}
		r.StartedAt, r.FinishedAt = time.Unix(st, 0), timeOf(fin)
		out = append(out, r)
	}
	return out, rows.Err()
}
