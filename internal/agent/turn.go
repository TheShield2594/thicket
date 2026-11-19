package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/theshield2594/thicket/internal/budget"
	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/llm"
	"github.com/theshield2594/thicket/internal/memory"
	"github.com/theshield2594/thicket/internal/store"
)

type handoffReq struct {
	to      string
	message string
}

// turn is the state of one character responding once.
type turn struct {
	user    *store.User
	char    *character.Character
	chatRef string // "" for scheduled jobs
	conv    *store.Conversation
	isDM    bool
	job     *store.Job
	depth   int
	query   string // text used to pick relevant memories
	// images and pdfs arrived with the user's newest message; they go to
	// the model with it, this turn only.
	images []llm.Image
	pdfs   []llm.File
	// pdfPages counts each PDF's pages, for the cost estimate.
	pdfPages []int
	// packages are sprig packages sent with the newest message.
	packages map[string][]byte
	// annotations are OpenRouter's parsed copies of pdfs from this turn.
	annotations []json.RawMessage
	// budgetChars overrides the history budget (after the model said its
	// context was full).
	budgetChars int
	// userMsgID is the stored message this turn answers (0 if none).
	userMsgID int64
	// mix is what the last prompt built was made of (for spend reports).
	mix llm.PromptMix

	toolCalls       int
	handoff         *handoffReq
	nothingToReport bool
	done            []string             // tools that ran, for a stop acknowledgement
	consult         *consult             // answering another sprig (ask_sprig)
	consults        int                  // ask_sprig calls this turn
	taskFrom        *character.Character // background task given by this sprig
	afterReply      []post               // canned posts after the reply (approval prompts)
	noTools         bool
	// spokenTo: the message this answers was a voice message (it was
	// transcribed), so a sprig that speaks when spoken to answers aloud.
	spokenTo bool

	// Costs (costs.go): costOK once the person agreed to this turn's
	// estimate; estimateID is the recorded estimate this turn's spend is
	// measured against; toolUSD adds up the tool estimates so far;
	// toolEstimate is the estimate for the tool call running now;
	// promptChars is the size of the turn's prompt.
	costOK       bool
	estimateID   int64
	toolUSD      float64
	toolEstimate int64
	promptChars  int

	// Taint (taint.go): the outside sources that entered this turn, and
	// the person's own words for it.
	taintedBy  []string
	userText   string // the newest user message, as stored
	request    string // set when the words come from another turn
	requestSet bool
	askedCache map[string]bool
}

type post struct {
	text     string
	approval *store.Approval      // registered only once the prompt is delivered
	as       *character.Character // who posts it (default: the turn's sprig)
}

func (t *turn) jobMode() bool { return t.job != nil }

// runTurn builds the prompt, runs the model/tool loop and returns the
// final reply text (already checked for character consistency).
func (e *Engine) runTurn(ctx context.Context, t *turn, event string) (string, error) {
	if f := inFlightFrom(ctx); f != nil && t.consult == nil {
		f.setChar(t.char.ID) // so pausing this sprig stops this work
	}
	if err := e.paused(ctx, t.char.ID); err != nil {
		return "", err
	}
	defer e.mark(t.char.ID, false)()
	msgs, err := e.buildMessages(ctx, t, event)
	if err != nil {
		return "", err
	}
	t.promptChars = llm.PromptChars(msgs)
	ctx, measured := e.meterTurn(ctx, t)
	defer measured()
	if err := e.checkTurnCost(ctx, t, msgs, t.char.Model); err != nil {
		return "", err
	}
	tools := e.toolsFor(ctx, t)
	defs := toolDefs(tools)
	if t.noTools {
		defs = nil
	}
	model := t.char.Model
	pdfEngine := e.Settings.String(ctx, "pdf_engine")
	if pdfEngine == "auto" {
		pdfEngine = ""
	}

	var reply string
	for i := 0; ; i++ {
		mix := t.mix
		req := llm.Request{Model: model, Messages: msgs, Tools: defs, MaxTokens: 1200, PDFEngine: pdfEngine, Mix: &mix}
		if i >= maxModelCallsPerTurn-1 || t.toolCalls >= maxToolCallsPerTurn {
			// Out of budget for tools: force a final answer.
			req.Tools = nil
			req.Messages = append(msgs, llm.Message{Role: "user", Content: "[Thicket event] Tool limit for this turn reached. Wrap up now: tell the user where things stand."})
		}
		resp, err := e.LLM.Chat(ctx, req)
		if err != nil && i == 0 && llm.ContextExceeded(err) && ctx.Err() == nil {
			// The model's context is smaller than the budget assumed:
			// retry with half the history that was sent (compaction then
			// catches up).
			budget := e.historyBudget(ctx, t)
			sent := 0
			for _, m := range msgs[1:] {
				sent += len(m.Content)
			}
			t.budgetChars = max(min(sent, budget)/2, minHistoryChars)
			e.Notify(ctx, "context:"+req.Model, 24*time.Hour, "warn", fmt.Sprintf("The model ran out of context, so I'm sending it less of the conversation (about %d tokens instead of %d). "+
				"If this keeps happening, ask me to set history_tokens lower.", t.budgetChars/4, budget/4))
			if msgs, err = e.buildMessages(ctx, t, event); err != nil {
				return "", err
			}
			req.Messages = msgs
			mix = t.mix
			resp, err = e.LLM.Chat(ctx, req)
		}
		if err != nil && llm.HasMedia(req.Messages) && llm.MediaRejected(err) && ctx.Err() == nil {
			// The model (and its fallback) won't take the images or PDFs:
			// carry on without them rather than fail the whole reply.
			e.logActivity(ctx, "warn", "model", t.char.ID, t.user.ID, "model refused the attached images or PDFs; replying without them", shortErr(err))
			msgs = withoutMedia(msgs)
			req.Messages = withoutMedia(req.Messages)
			resp, err = e.LLM.Chat(ctx, req)
		}
		if err != nil && req.Tools != nil && llm.ToolsRejected(err) && ctx.Err() == nil {
			// Small local models often can't take tool definitions: answer
			// without tools rather than not at all, and say what's lost.
			model := req.Model
			if model == "" {
				model = e.Settings.String(ctx, "model")
			}
			e.Notify(ctx, "notools:"+model, 24*time.Hour, "warn", fmt.Sprintf("The model %s can't use tools, so sprigs answer without them: they can't remember things, schedule jobs or use integrations. "+
				"A model that supports tools (like qwen2.5 or llama3.1 on Ollama) fixes that: ask me to switch.", model))
			defs = nil
			req.Tools = nil
			resp, err = e.LLM.Chat(ctx, req)
		}
		if err != nil {
			return "", err
		}
		m := resp.Message
		if len(m.Annotations) > 0 {
			t.annotations = append(t.annotations, m.Annotations)
		}
		if len(m.ToolCalls) == 0 || req.Tools == nil {
			reply = m.Content
			break
		}
		// Annotations carry parsed PDFs, so later calls skip re-parsing.
		msgs = append(msgs, llm.Message{Role: "assistant", Content: m.Content, ToolCalls: m.ToolCalls, Annotations: m.Annotations})
		for _, call := range m.ToolCalls {
			result := e.executeTool(ctx, t, tools, call)
			msgs = append(msgs, llm.Message{Role: "tool", ToolCallID: call.ID, Content: result})
		}
	}
	reply = cleanReply(reply, t.char)
	if t.nothingToReport && t.jobMode() {
		return "", nil
	}
	if reply == "" {
		return "", nil
	}
	return e.ensureInCharacter(ctx, t, msgs[0].Content, reply), nil
}

func (e *Engine) buildMessages(ctx context.Context, t *turn, event string) ([]llm.Message, error) {
	mems, err := e.memoriesFor(ctx, t)
	if err != nil {
		return nil, err
	}
	var history []store.Message
	summary, pinned := "", ""
	if t.conv != nil {
		if history, err = e.DB.MessagesAfter(ctx, t.conv.ID, t.conv.SummaryUpto, historyLoadLimit); err != nil {
			return nil, err
		}
		pins, err := e.DB.PinnedMessages(ctx, t.conv.ID)
		if err != nil {
			return nil, err
		}
		// Pinned messages that would fall out of the history are shown on
		// their own, and take their room from the history budget.
		budget := e.historyBudget(ctx, t)
		pinned = e.pinnedBlock(pins, nil)
		history, _ = fitHistory(history, max(budget-len(pinned), minHistoryChars), t.userMsgID)
		pinned = e.pinnedBlock(pins, history)
		summary = t.conv.Summary
	}
	query := t.query + " " + event
	for i := len(history) - 1; i >= 0 && i >= len(history)-4; i-- {
		query += " " + history[i].Content
	}
	// Relationship memory has its own room, so facts never crowd out how
	// the person likes to be spoken to.
	facts, rel := memory.Split(mems)
	selected := append(memory.Select(facts, query, e.now(), 25, 3500), memory.Select(rel, query, e.now(), 15, 1500)...)
	ids := make([]int64, len(selected))
	for i, m := range selected {
		ids[i] = m.ID
	}
	_ = e.DB.MarkMemoriesUsed(ctx, ids)

	skills, err := e.DB.Skills(ctx)
	if err != nil {
		return nil, err
	}
	in := promptInput{
		skills: skills,
		char:   t.char, user: t.user, roster: e.Chars.For(t.user), memories: selected, now: e.now(),
		jobMode: t.jobMode(), onboarded: !t.user.OnboardedAt.IsZero(),
		hostTools: t.char.HasScope(character.ScopeCharacters), isDM: t.isDM, commons: e.isCommons(t.chatRef),
		web: strings.HasPrefix(t.chatRef, "web:") || (t.chatRef == "" && t.user.Channel == "web"),
	}
	if t.char.Host && t.user.IsAdmin() {
		in.sandbox, in.sandboxIdle = e.sandboxState()
	}
	sys := buildSystemPrompt(in)
	msgs := []llm.Message{{Role: "system", Content: sys}}
	t.mix = llm.PromptMix{}
	if summary != "" {
		msgs = append(msgs, llm.Message{Role: "user", Content: summaryMessage(summary)})
		t.mix.HistoryChars += len(msgs[len(msgs)-1].Content)
	}
	if pinned != "" {
		msgs = append(msgs, llm.Message{Role: "user", Content: pinned})
		t.mix.HistoryChars += len(pinned)
	}
	for i, h := range history {
		// The message this turn answers, tracked by id: a notice added to
		// the conversation mid-turn mustn't take its media or its room.
		origin := h.Role == "user" && (h.ID == t.userMsgID || (t.userMsgID == 0 && i == len(history)-1))
		content := h.Content
		// It may carry text files the user just sent: let the model read
		// them all this once (each file is capped, plus its wrapper).
		limit := 4000
		if origin {
			limit = maxFilesPerMessage*(maxTextFileChars+200) + 4000
			if t.budgetChars > 0 {
				// The model said its context was full: this message must
				// shrink too, not just the history before it.
				limit = min(limit, max(t.budgetChars, minHistoryChars))
			}
		}
		if len(content) > limit && h.Role != "document" { // documents are capped when stored
			cut := limit
			for cut > 0 && !utf8.RuneStart(content[cut]) {
				cut--
			}
			content = content[:cut] + " […cut to fit]"
		}
		if origin && h.Role == "user" {
			t.userText = h.Content
		}
		switch {
		case h.Role == "user":
			m := llm.Message{Role: "user", Content: content}
			if origin {
				m.Images, m.Files = t.images, t.pdfs
			}
			msgs = append(msgs, m)
		case h.Role == "assistant" && h.Character == t.char.ID:
			msgs = append(msgs, llm.Message{Role: "assistant", Content: content})
		case h.Role == "assistant":
			msgs = append(msgs, llm.Message{Role: "user", Content: fmt.Sprintf("[%s said to the user]: %s", e.charName(h.Character), content)})
		case h.Role == "document":
			msgs = append(msgs, llm.Message{Role: "user", Content: documentMessage(h.Content)})
		case h.Role == "event":
			msgs = append(msgs, llm.Message{Role: "user", Content: "[Thicket event] " + content})
		default:
			continue
		}
		// What this turn answers isn't history; everything before it is
		// re-sent with each reply.
		switch n := len(msgs[len(msgs)-1].Content); {
		case h.Role == "document":
			t.mix.DocumentChars += n
		case !origin:
			t.mix.HistoryChars += n
		}
	}
	if t.jobMode() {
		task := fmt.Sprintf("[Scheduled job %q]\nTask: %s", t.job.Name, t.job.Prompt)
		switch {
		case t.taskFrom != nil:
			task = fmt.Sprintf("[Background task from %s]\nDo this, then write the message the user should get with the result:\n%s", t.taskFrom.DisplayName(), t.job.Prompt)
		case t.job.Kind == store.JobWebhook || t.job.Payload != "":
			task = e.webhookTask(t.job)
		}
		msgs = append(msgs, llm.Message{Role: "user", Content: task})
	}
	if t.consult != nil {
		msgs = append(msgs, llm.Message{Role: "user", Content: consultMessage(t.consult)})
	}
	if event != "" {
		msgs = append(msgs, llm.Message{Role: "user", Content: "[Thicket event] " + event})
	}
	if t.jobMode() && t.taskFrom == nil && (t.job.Kind == store.JobWebhook || t.job.Payload != "") {
		t.addTaint("webhook or event: " + t.job.Name) // tainted from the start
	}
	for _, m := range msgs[1:] {
		t.taintFrom(m.Content)
	}
	for _, f := range t.pdfs {
		t.addTaint("pdf " + f.Name) // sent as a file, not as wrapped text
	}
	// Models need the conversation to end on a user turn.
	if last := msgs[len(msgs)-1]; last.Role != "user" {
		msgs = append(msgs, llm.Message{Role: "user", Content: "[Thicket event] Continue."})
	}
	return msgs, nil
}

func (e *Engine) charName(id string) string {
	if c, ok := e.Chars.Get(id); ok {
		return c.DisplayName()
	}
	return id
}

// cleanReply strips a leading "Name:" the model sometimes adds.
func cleanReply(s string, c *character.Character) string {
	s = strings.TrimSpace(s)
	for _, n := range []string{c.Name, c.Nickname} {
		if n == "" {
			continue
		}
		for _, p := range []string{n + ":", "**" + n + ":**", "**" + n + "**:"} {
			if strings.HasPrefix(s, p) {
				s = strings.TrimSpace(s[len(p):])
			}
		}
	}
	return s
}

// executeTool runs one tool call with permission, approval and loop checks.
// Its return value goes back to the model.
func (e *Engine) executeTool(ctx context.Context, t *turn, tools map[string]*Tool, call llm.ToolCall) string {
	t.toolCalls++
	name := call.Function.Name
	if t.toolCalls > maxToolCallsPerTurn {
		return "Error: tool call limit for this turn reached. Stop calling tools and reply."
	}
	tool, ok := tools[name]
	if !ok {
		// Not offered to this character: enforced in code, not by prompt.
		e.logActivity(ctx, "warn", "tool", t.char.ID, t.user.ID, "blocked call to unavailable tool "+name, "")
		return fmt.Sprintf("Error: you don't have access to a tool called %q.", name)
	}
	args := json.RawMessage(call.Function.Arguments)
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	if !json.Valid(args) {
		return "Error: tool arguments were not valid JSON."
	}
	if err := e.paused(ctx, t.char.ID); err != nil {
		e.logActivity(ctx, "warn", "tool", t.char.ID, t.user.ID, "refused "+name+": "+err.Error(), "")
		return "Error: " + err.Error() + ", so " + tool.Label() + " was NOT run. Stop and tell the user."
	}
	if msg, ok := e.gate(ctx, t, tool, args); !ok {
		return msg
	}
	if msg := e.rateLimited(ctx, t, tool, true); msg != "" {
		return msg
	}
	t.toolEstimate = 0
	if msg, ok := e.costGate(ctx, t, tool, args); !ok {
		return msg
	}
	done := e.mark(t.char.ID, true)
	runCtx, finish := ctx, func() {}
	if id := t.toolEstimate; id != 0 && tool.Name != "delegate_task" { // a background task is measured where it runs
		var m *budget.Meter
		runCtx, m = budget.WithMeter(ctx)
		finish = func() { _ = e.DB.FinishEstimate(context.WithoutCancel(ctx), id, m.USD()) }
	}
	out, err := tool.Run(runCtx, t, args)
	finish()
	done()
	level, summary := "info", "called "+name
	if err != nil {
		level, summary = "warn", "tool "+name+" failed"
		out = "Error: " + err.Error()
	} else {
		t.done = append(t.done, tool.Label())
	}
	detail := e.Secrets.Redact(truncate(string(args), 500) + " → " + truncate(out, 500))
	if e.privateDraft(t, name, args) {
		detail = "(a private sprig's draft: not logged)" // admins read the activity log
	}
	e.logActivity(ctx, level, "tool", t.char.ID, t.user.ID, summary, detail)
	// Tool output goes back to the model: no secret values in it.
	out = e.Secrets.Redact(out)
	if tool.Untrusted {
		t.addTaint(tool.Label())
		return wrapUntrusted(name, out)
	}
	return out
}

// privateDraft reports whether a tool call drafts or proposes a private
// sprig, whose details stay out of the activity log.
func (e *Engine) privateDraft(t *turn, name string, args json.RawMessage) bool {
	switch name {
	case "preview_character":
		var a struct {
			Private bool `json:"private"`
		}
		return json.Unmarshal(args, &a) == nil && a.Private
	case "create_character":
		v, ok := e.drafts.Load(t.user.ID)
		return ok && v.(*sprigDraft).char.BelongsTo != 0
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// deliverApproval shows the prompt and only then registers the approval.
// An approval that exists is one the user has seen, so a stray "ok" can
// never run an action whose details weren't shown, whatever failed.
func (e *Engine) deliverApproval(ctx context.Context, c *character.Character, a *store.Approval, msg string) error {
	if err := e.send(ctx, a.ChatRef, c, msg); err != nil {
		return err
	}
	// Register even if the caller's context ended right after delivery.
	id, err := e.DB.CreateApproval(context.WithoutCancel(ctx), *a)
	if err != nil {
		e.Log.Error("register approval", "err", err)
		_ = e.send(context.WithoutCancel(ctx), a.ChatRef, c, "Sorry, I couldn't register that request, so approving it won't do anything. Please ask me again.")
		return err
	}
	e.logActivity(ctx, "info", "approval", c.ID, a.UserID, fmt.Sprintf("approval #%d shown for %s", id, a.Tool), "")
	e.noteWaiting(c.ID) // unsure until answered (or the mood fades)
	return nil
}

func (e *Engine) requestApproval(ctx context.Context, t *turn, tool *Tool, args json.RawMessage) string {
	return e.requestApprovalNote(ctx, t, tool, args, "")
}

// requestApprovalNote is requestApproval with a line under the details
// (what it will cost, or that it can't be undone).
func (e *Engine) requestApprovalNote(ctx context.Context, t *turn, tool *Tool, args json.RawMessage, note string) string {
	chatRef := t.chatRef
	if chatRef == "" {
		// Jobs have no chat: ask in DMs.
		ref, err := e.DB.LastChatRef(ctx, t.user.ID)
		if err != nil || ref == "" {
			return "Error: this action needs the user's approval, but there's no chat to ask in."
		}
		chatRef = ref
	}
	pretty := prettyArgs(args)
	if note != "" {
		note += "\n"
	}
	msg := fmt.Sprintf("🔒 I need your OK before I do this:\n**%s**\n```json\n%s\n```\n%sReply **approve** or **deny**.", tool.Label(), truncate(pretty, 1500), note)
	a := &store.Approval{UserID: t.user.ID, Character: t.char.ID, ChatRef: chatRef, Tool: tool.Name, Args: string(args)}
	if t.chatRef != "" {
		// Posted after the reply; registered by respond once delivered.
		t.afterReply = append(t.afterReply, post{text: msg, approval: a})
	} else if err := e.deliverApproval(ctx, t.char, a, msg+e.mustReach(ctx, t.char, t.user, "it needs your approval")); err != nil {
		// A job's approval request bypasses quiet hours and the daily
		// budget: the job is waiting on it.
		return "Error: couldn't ask the user for approval: " + err.Error()
	}
	e.logActivity(ctx, "info", "approval", t.char.ID, t.user.ID, "approval requested for "+tool.Name, string(args))
	return "PENDING: this action needs the user's approval. A request with the exact details has been shown to them. Tell them briefly what you want to do and that you're waiting for their OK. Do not say it's done."
}

// ensureInCharacter runs the personality consistency check: a code check
// for banned phrases, then (if enabled) a cheap-model comparison against
// the identity. One rewrite is attempted on failure.
func (e *Engine) ensureInCharacter(ctx context.Context, t *turn, system, reply string) string {
	problems, mood := bannedPhrases(t.char, reply), ""
	if len(problems) == 0 && e.Settings.Bool(ctx, "consistency_check") && len(reply) > 60 {
		if issue, m, err := e.voiceCheck(ctx, t.char, reply); err != nil {
			e.Log.Warn("consistency check failed to run", "err", err)
		} else if mood = m; issue != "" {
			problems = append(problems, issue)
		}
	}
	e.noteReply(t, len(problems) == 0, mood) // for the avatar's mood
	if len(problems) == 0 {
		return reply
	}
	e.logActivity(ctx, "info", "character", t.char.ID, t.user.ID, "reply drifted from character; rewriting", strings.Join(problems, "; "))
	resp, err := e.LLM.Chat(llm.WithPurpose(ctx, "consistency"), llm.Request{
		Model: t.char.Model,
		Messages: []llm.Message{
			{Role: "system", Content: system},
			{Role: "user", Content: "[Thicket event] Your draft reply drifted out of character. Problems: " + strings.Join(problems, "; ") +
				"\nRewrite it in your own voice, keeping the same facts and meaning. Reply with the rewritten message only.\n\nDraft:\n" + reply},
		},
		MaxTokens: 1200,
	})
	if err != nil {
		return reply
	}
	fixed := cleanReply(resp.Message.Content, t.char)
	if fixed == "" || len(bannedPhrases(t.char, fixed)) > 0 {
		return reply
	}
	return fixed
}

var genericBanned = []string{"as an ai language model", "as a large language model", "i am an ai assistant", "i'm an ai assistant"}

func bannedPhrases(c *character.Character, reply string) []string {
	low := strings.ToLower(reply)
	var out []string
	for _, p := range append(append([]string{}, c.Voice.NeverSay...), genericBanned...) {
		if p = strings.ToLower(strings.TrimSpace(p)); p != "" && strings.Contains(low, p) {
			out = append(out, fmt.Sprintf("used a banned phrase: %q", p))
		}
	}
	return out
}

// voiceCheck asks the cheap model whether reply is in character and, in
// the same call, how it reads (pleased or unsure, for the avatar's mood).
func (e *Engine) voiceCheck(ctx context.Context, c *character.Character, reply string) (issue, mood string, err error) {
	temp := 0.0
	identity := fmt.Sprintf("Name: %s\nRole: %s\nTraits: %s\nVoice rules:\n- %s\nNever says: %s",
		c.DisplayName(), c.Role, strings.Join(c.Personality.Traits, ", "), strings.Join(c.Voice.Style, "\n- "), strings.Join(c.Voice.NeverSay, " | "))
	resp, err := e.LLM.Chat(llm.WithPurpose(ctx, "consistency"), llm.Request{
		Model: e.Settings.String(ctx, "cheap_model"),
		Messages: []llm.Message{
			{Role: "system", Content: "You check whether a chat reply matches a character's identity. Be lenient: only flag clear breaks (generic assistant tone, wrong personality, breaking voice rules, claiming to be a different character). Also read the reply's mood: \"unsure\" if it refuses, can't help, hedges, apologises or is waiting on the user, else \"pleased\". Reply with JSON only: {\"in_character\": true|false, \"issue\": \"short reason if false\", \"mood\": \"pleased\"|\"unsure\"}"},
			{Role: "user", Content: "Character:\n" + identity + "\n\nReply to check:\n" + reply},
		},
		Temperature: &temp, MaxTokens: 150, JSON: true, NoFallback: true,
	})
	if err != nil {
		return "", "", err
	}
	var v struct {
		InCharacter *bool  `json:"in_character"`
		Issue       string `json:"issue"`
		Mood        string `json:"mood"`
	}
	s := strings.TrimSpace(resp.Message.Content)
	s = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(s, "```json"), "```"), "```")
	if err := json.Unmarshal([]byte(strings.TrimSpace(s)), &v); err != nil || v.InCharacter == nil {
		return "", "", fmt.Errorf("unreadable consistency verdict")
	}
	if *v.InCharacter {
		return "", v.Mood, nil
	}
	if v.Issue == "" {
		v.Issue = "doesn't sound like " + c.DisplayName()
	}
	return v.Issue, v.Mood, nil
}
