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
var teamTools = map[string]bool{"ask_sprig": true, "handoff": true, "delegate_task": true, "request_access": true}

// findSprig finds a sprig the turn's user may talk to.
func (e *Engine) findSprig(t *turn, name string) (*character.Character, error) {
	c, ok := e.Chars.Get(strings.ToLower(strings.TrimSpace(name)))
	if !ok {
		c, ok = e.Chars.FindByName(name)
	}
	if !ok || (t != nil && t.user != nil && !c.UsableBy(t.user)) {
		return nil, fmt.Errorf("there's no sprig called %q", name)
	}
	return c, nil
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
	target, err := e.findSprig(t, a.To)
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
	sub.inherit(t)
	sub.addTaint("sprig " + t.char.ID) // the question is a sprig's words
	sctx := llm.WithMeta(ctx, llm.Meta{UserID: t.user.ID, Character: target.ID, JobID: jobID(t), Purpose: "consult", Origin: llm.OriginOf(llm.MetaFrom(ctx))})
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
			t.char.DisplayName(), target.DisplayName(), q, target.DisplayName(), wrapUntrusted("sprig "+target.ID, truncate(answer, 2000))))
	}
	e.logActivity(ctx, "info", "sprig", t.char.ID, t.user.ID, fmt.Sprintf("%s asked %s a question", t.char.DisplayName(), target.DisplayName()), q)
	t.addTaint("sprig " + target.ID)
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
		if target, err = e.findSprig(t, a.To); err != nil {
			return "", err
		}
	}
	task := strings.TrimSpace(a.Task)
	if task == "" || len(task) > 2000 {
		return "", errors.New("task is required (max 2000 characters): say what to do and what to report")
	}
	// Check the cap and take a slot in one step, so two calls at once
	// can't both squeeze in.
	e.taskMu.Lock()
	if e.taskUse == nil {
		e.taskUse = map[int64]int{}
	}
	if n := e.taskUse[t.user.ID]; n >= maxTasksPerUser {
		e.taskMu.Unlock()
		return "", fmt.Errorf("%d background tasks are already running for this user: wait for one to finish (or they can say stop)", n)
	}
	e.taskUse[t.user.ID]++
	e.taskMu.Unlock()
	from := t.char
	name := fmt.Sprintf("%s's task: %s", target.DisplayName(), truncate(task, 60))
	// Stored first, so a restart mid-task is reported, not lost.
	taskID, err := e.DB.StartTask(ctx, store.Task{UserID: t.user.ID, From: from.ID, Character: target.ID, Prompt: task})
	if err != nil {
		e.taskMu.Lock()
		e.taskUse[t.user.ID]--
		e.taskMu.Unlock()
		return "", fmt.Errorf("couldn't record the task: %w", err)
	}
	// The task carries this turn's taint and the person's words; the task
	// itself is a sprig's words.
	origin := &turn{}
	origin.inherit(t)
	origin.addTaint("sprig " + from.ID)
	e.tasks.Add(1)
	go func() {
		defer e.tasks.Done()
		defer func() {
			e.taskMu.Lock()
			e.taskUse[t.user.ID]--
			e.taskMu.Unlock()
		}()
		e.runTask(context.WithoutCancel(ctx), taskID, t.user, from, target, task, name, origin)
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
func (e *Engine) runTask(ctx context.Context, taskID int64, user *store.User, from, target *character.Character, task, name string, origin *turn) {
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
	if origin != nil {
		t.inherit(origin)
	}
	out, err := e.runTurn(llm.WithMeta(ctx, llm.Meta{UserID: user.ID, Character: target.ID, Purpose: "task"}), t, "")
	bg := context.WithoutCancel(ctx)
	ref := fmt.Sprintf("task:%d", taskID)
	switch {
	case life != nil && life.Err() != nil:
		return // shutting down: left running, reported as interrupted at the next start
	case flight.stopped.Load():
		_ = e.DB.FinishTask(bg, taskID, "stopped", "")
		e.logActivity(bg, "info", "sprig", target.ID, user.ID, name+": stopped by the user", "")
		return
	case err != nil:
		_ = e.DB.FinishTask(bg, taskID, "error", err.Error())
		e.logActivity(bg, "error", "sprig", target.ID, user.ID, name+": failed", err.Error())
		e.deadLetter(bg, store.InboxItem{UserID: user.ID, Character: target.ID, Kind: store.InboxFailed, Ref: ref,
			Title: fmt.Sprintf("%s couldn't finish a task from %s", target.DisplayName(), from.DisplayName()), Detail: "Task: " + task + "\n\nError: " + err.Error()})
		out = fmt.Sprintf("⚠️ I couldn't finish the task %s gave me (%s): %s", from.DisplayName(), truncate(task, 120), shortErr(err))
	case t.nothingToReport || strings.TrimSpace(out) == "":
		_ = e.DB.FinishTask(bg, taskID, "empty", "")
		out = fmt.Sprintf("I finished the task %s gave me (%s): nothing worth reporting.", from.DisplayName(), truncate(task, 120))
	default:
		_ = e.DB.FinishTask(bg, taskID, "ok", "")
		e.logActivity(bg, "info", "sprig", target.ID, user.ID, name+": done", "")
	}
	if err := e.sendToUser(bg, user, target, out); err != nil {
		e.Log.Error("deliver task result", "err", err)
		e.deadLetter(bg, store.InboxItem{UserID: user.ID, Character: target.ID, Kind: store.InboxUndelivered, Ref: ref,
			Title: fmt.Sprintf("%s finished a task, but the message couldn't be delivered", target.DisplayName()), Detail: out})
	}
}

// RecoverInterrupted fails the delegated tasks a stop or crash cut short
// and puts each in the inbox. Call it at startup, before any new task can
// start. They're failed cleanly rather than resumed: a half-done task may
// already have acted, and running it again could do things twice. Pass
// the result to ReportInterrupted once the chat apps are connected.
func (e *Engine) RecoverInterrupted(ctx context.Context) []store.Task {
	ctx = context.WithoutCancel(ctx) // a stop during startup mustn't cut recovery short
	tasks, err := e.DB.InterruptRunningTasks(ctx, func(tk store.Task) store.InboxItem {
		return store.InboxItem{UserID: tk.UserID, Character: tk.Character, Kind: store.InboxInterrupted, Ref: fmt.Sprintf("task:%d", tk.ID),
			Title:  fmt.Sprintf("%s's task was interrupted by a restart", e.charName(tk.Character)),
			Detail: e.Secrets.Redact(truncate("Task: "+tk.Prompt, 4000))}
	})
	if err != nil {
		e.Log.Error("recover interrupted tasks; will retry at the next start", "err", err)
		return nil
	}
	for _, tk := range tasks {
		e.logActivity(ctx, "warn", "sprig", tk.Character, tk.UserID, fmt.Sprintf("task #%d interrupted by a restart", tk.ID), tk.Prompt)
	}
	return tasks
}

// ReportInterrupted has each interrupted task's sprig tell its user.
func (e *Engine) ReportInterrupted(ctx context.Context, tasks []store.Task) {
	for _, tk := range tasks {
		user, err := e.DB.UserByID(ctx, tk.UserID)
		if err != nil {
			continue
		}
		char, ok := e.Chars.Get(tk.Character)
		if !ok {
			if char, ok = e.Chars.Host(); !ok {
				continue
			}
		}
		msg := fmt.Sprintf("⚠️ Thicket restarted while I was working on a task from %s (%s), so it didn't finish. Ask me again if you still want it.", e.charName(tk.From), truncate(tk.Prompt, 120))
		if err := e.sendToUser(ctx, user, char, msg); err != nil {
			e.Log.Error("report interrupted task", "task", tk.ID, "err", err) // it's in the inbox regardless
		}
	}
}

// WaitTasks waits for background tasks to finish (tests, shutdown).
func (e *Engine) WaitTasks() { e.tasks.Wait() }
