package store

import (
	"context"
	"time"
)

// Task is a background task one sprig gave another (delegate_task). It's
// stored so a restart can't lose it silently.
type Task struct {
	ID         int64
	UserID     int64
	From       string
	Character  string
	Prompt     string
	Status     string // running | ok | empty | error | stopped | interrupted
	Error      string
	StartedAt  time.Time
	FinishedAt time.Time
}

func (s *Store) StartTask(ctx context.Context, t Task) (int64, error) {
	res, err := s.db.ExecContext(ctx, "INSERT INTO tasks (user_id, from_char, character, prompt, status, started_at) VALUES (?, ?, ?, ?, 'running', ?)",
		t.UserID, t.From, t.Character, t.Prompt, s.unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) FinishTask(ctx context.Context, id int64, status, errText string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE tasks SET status = ?, error = ?, finished_at = ? WHERE id = ? AND status = 'running'", status, errText, s.unix(), id)
	return err
}

// InterruptRunningTasks marks tasks left running by a stop or crash as
// interrupted and, in the same transaction, files the inbox item report
// builds for each. Either both happen or neither: a task is never left
// interrupted without its report. On error nothing changes and the tasks
// are found again at the next start.
func (s *Store) InterruptRunningTasks(ctx context.Context, report func(Task) InboxItem) ([]Task, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, "SELECT id, user_id, from_char, character, prompt, started_at FROM tasks WHERE status = 'running' ORDER BY id")
	if err != nil {
		return nil, err
	}
	var out []Task
	for rows.Next() {
		var t Task
		var st int64
		if err := rows.Scan(&t.ID, &t.UserID, &t.From, &t.Character, &t.Prompt, &st); err != nil {
			rows.Close()
			return nil, err
		}
		t.StartedAt = time.Unix(st, 0)
		t.Status = "interrupted"
		out = append(out, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	now := s.unix()
	for _, t := range out {
		if _, err := tx.ExecContext(ctx, "UPDATE tasks SET status = 'interrupted', error = ?, finished_at = ? WHERE id = ? AND status = 'running'",
			"Thicket stopped while this task was in progress", now, t.ID); err != nil {
			return nil, err
		}
		if report == nil {
			continue
		}
		it := report(t)
		if _, err := tx.ExecContext(ctx, `INSERT INTO inbox (user_id, character, kind, ref, title, detail, action, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, it.UserID, it.Character, it.Kind, it.Ref, it.Title, it.Detail, it.Action, now, now); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}
