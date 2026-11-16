package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/llm"
	"github.com/theshield2594/thicket/internal/store"
)

// Sprigs working together, beyond handing the conversation over:
//
//   - ask_sprig: ask a fellow sprig something mid-reply and get its answer
//     back. The asked sprig answers in its own voice with its own memories
//     and tools, but can't ask others, hand off or delegate in turn.
//   - delegate_task: give a fellow sprig (or yourself) longer work to do in
//     the background; it messages the user when done. "stop" stops it.

const (
	maxConsultsPerTurn = 2
	maxTasksPerUser    = 3
	taskTimeout        = 10 * time.Minute
)

// consult marks a turn that answers another sprig instead of the user.
type consult struct {
	from     *character.Character
	question string
}

// teamTools are what an asked sprig can't use: no chains of sprigs.
var teamTools = map[string]bool{"ask_sprig": true, "handoff": true, "delegate_task": true}

func (e *Engine) findSprig(name string) (*character.Character, error) {
	if c, ok := e.Chars.Get(strings.ToLower(strings.TrimSpace(name))); ok {
		return c, nil
	}
	if c, ok := e.Chars.FindByName(name); ok {
		return c, nil
	}
	return nil, fmt.Errorf("there's no sprig called %q", name)
}

func (e *Engine) toolAskSprig(ctx context.Context, t *turn, args json.RawMessage) (string, error) {
	a, err := decode[struct {
		To       string `json:"to"`
		Question string `json:"question"`
	}](args)
	if err != nil {
		return "", err
	}
	if t.consult != nil {
		return "", errors.New("you're answering another sprig, so you can't ask others in turn: answer with what you know")
	}
	target, err := e.findSprig(a.To)
	if err != nil {
		return "", err
	}
	if target.ID == t.char.ID {
		return "", errors.New("that's you: answer it yourself")
	}
	q := strings.TrimSpace(a.Question)
	if q == "" || len(q) > 2000 {
		return "", errors.New("question is required (max 2000 characters)")
	}
	if t.consults >= maxConsultsPerTurn {
		return "", fmt.Errorf("you've already asked %d sprigs this turn: reply with what you have", maxConsultsPerTurn)
	}
	t.consults++

	// No job and no conversation: the asked sprig sees only the question
	// (approvals it raises go out with this reply, or by DM in a job).
	sub := &turn{user: t.user, char: target, chatRef: t.chatRef, isDM: t.isDM,
		consult: &consult{from: t.char, question: q}, query: q, depth: t.depth}
	sctx := llm.WithMeta(ctx, llm.Meta{UserID: t.user.ID, Character: target.ID, JobID: jobID(t), Purpose: "consult"})
	answer, err := e.runTurn(sctx, sub, "")
	// Approval prompts the asked sprig raised go out with this reply.
	t.afterReply = append(t.afterReply, sub.afterReply...)
	t.done = append(t.done, sub.done...)
	if err != nil {
		return "", fmt.Errorf("%s couldn't answer: %s", target.DisplayName(), shortErr(err))
	}
	if strings.TrimSpace(answer) == "" {
		answer = "(no answer)"
	}
	if t.conv != nil {
		_ = e.DB.AddMessage(ctx, t.conv.ID, "event", "", fmt.Sprintf("%s asked %s: %s\n%s answered: %s",
			t.char.DisplayName(), target.DisplayName(), q, target.DisplayName(), truncate(answer, 2000)))
	}
	e.logActivity(ctx, "info", "sprig", t.char.ID, t.user.ID, fmt.Sprintf("%s asked %s a question", t.char.DisplayName(), target.DisplayName()), q)
	return fmt.Sprintf("%s answered (pass on what matters, in your own words):\n%s", target.DisplayName(), wrapUntrusted("sprig "+target.ID, answer)), nil
}

func jobID(t *turn) int64 {
	if t.job != nil {
		return t.job.ID
	}
	return 0
}

// consultMessage is the asked sprig's view of the question.
func consultMessage(c *consult) string {
	return fmt.Sprintf("[Thicket event] Your fellow sprig %s is asking you this, on the user's behalf:\n%s\n\n"+
		"Answer %s directly and briefly; they'll pass on what matters to the user. Use your tools if you need to. Don't greet the user or sign off.",
		c.from.DisplayName(), c.question, c.from.DisplayName())
}

// --- background tasks ---

func (e *Engine) toolDelegateTask(ctx context.Context, t *turn, args json.RawMessage) (string, error) {
	a, err := decode[struct {
		To   string `json:"to"`
		Task string `json:"task"`
	}](args)
	if err != nil {
		return "", err
	}
	if t.consult != nil {
		return "", errors.New("you're answering another sprig, so you can't hand out work")
	}
	target := t.char
	if a.To != "" {
		if target, err = e.findSprig(a.To); err != nil {
			return "", err
		}
	}
	task := strings.TrimSpace(a.Task)
	if task == "" || len(task) > 2000 {
		return "", errors.New("task is required (max 2000 characters): say what to do and what to report")
	}
	running := 0
	e.jobsRunning.Range(func(k, v any) bool {
		if _, isTask := k.(string); isTask && v.(*inFlight).userID == t.user.ID {
			running++
		}
		return true
	})
	if running >= maxTasksPerUser {
		return "", fmt.Errorf("%d background tasks are already running for this user: wait for one to finish (or they can say stop)", running)
	}
	from := t.char
	name := fmt.Sprintf("%s's task: %s", target.DisplayName(), truncate(task, 60))
	e.tasks.Add(1)
	go func() {
		defer e.tasks.Done()
		e.runTask(context.WithoutCancel(ctx), t.user, from, target, task, name)
	}()
	e.logActivity(ctx, "info", "sprig", from.ID, t.user.ID, fmt.Sprintf("%s gave %s a background task", from.DisplayName(), target.DisplayName()), task)
	who := target.DisplayName() + " is"
	if target.ID == from.ID {
		who = "You're"
	}
	return fmt.Sprintf("%s working on it in the background now and will message the user when done (the user can say stop to cancel it). Tell the user briefly; don't do the task yourself.", who), nil
}

// SetLifetime ties background tasks to the app: when ctx ends, they stop.
func (e *Engine) SetLifetime(ctx context.Context) {
	e.lifeMu.Lock()
	e.lifetime = ctx
	e.lifeMu.Unlock()
}

// runTask does delegated work like a one-off job and delivers the result.
func (e *Engine) runTask(ctx context.Context, user *store.User, from, target *character.Character, task, name string) {
	e.lifeMu.Lock()
	life := e.lifetime
	e.lifeMu.Unlock()
	if life != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
		defer context.AfterFunc(life, cancel)()
	}
	ctx, flight, done := e.track(ctx, &e.jobsRunning, fmt.Sprintf("task:%d", e.taskSeq.Add(1)), user.ID, name)
	defer done()
	ctx, cancel := context.WithTimeout(ctx, taskTimeout)
	defer cancel()
	job := &store.Job{UserID: user.ID, Character: target.ID, Name: name, Prompt: task, Kind: "task"}
	t := &turn{user: user, char: target, job: job, query: task, taskFrom: from}
	out, err := e.runTurn(llm.WithMeta(ctx, llm.Meta{UserID: user.ID, Character: target.ID, Purpose: "task"}), t, "")
	bg := context.WithoutCancel(ctx)
	switch {
	case life != nil && life.Err() != nil:
		return // Thicket is shutting down
	case flight.stopped.Load():
		e.logActivity(bg, "info", "sprig", target.ID, user.ID, name+": stopped by the user", "")
		return
	case err != nil:
		e.logActivity(bg, "error", "sprig", target.ID, user.ID, name+": failed", err.Error())
		out = fmt.Sprintf("⚠️ I couldn't finish the task %s gave me (%s): %s", from.DisplayName(), truncate(task, 120), shortErr(err))
	case t.nothingToReport || strings.TrimSpace(out) == "":
		out = fmt.Sprintf("I finished the task %s gave me (%s): nothing worth reporting.", from.DisplayName(), truncate(task, 120))
	default:
		e.logActivity(bg, "info", "sprig", target.ID, user.ID, name+": done", "")
	}
	if err := e.sendToUser(bg, user, target, out); err != nil {
		e.Log.Error("deliver task result", "err", err)
	}
}

// WaitTasks waits for background tasks to finish (tests, shutdown).
func (e *Engine) WaitTasks() { e.tasks.Wait() }
