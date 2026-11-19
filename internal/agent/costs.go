package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/theshield2594/thicket/internal/budget"
	"github.com/theshield2594/thicket/internal/channel"
	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/llm"
	"github.com/theshield2594/thicket/internal/settings"
	"github.com/theshield2594/thicket/internal/store"
)

// Spend before it happens. Before something costly (a long PDF read with
// OCR, a background task, many web searches, a long conversation on a big
// model) Thicket estimates the cost from token counts and known per-unit
// prices:
//
//   - above budget.ask_above_usd it says the figure and asks first;
//   - if it would cross a cap that applies to the sprig (its own daily or
//     monthly cap, or the household's), it refuses and says by how much.
//
// Each estimate is recorded with what the action really cost (store
// estimates), so `thicket costs` can show how good the estimates are.
//
// Per-sprig caps: budget.character.<id>.daily_usd and .monthly_usd. A kid
// sprig starts with budget.kid_monthly_usd a month, and only its parents
// can raise it. Cap changes go in the trail, with undo.

const (
	// fallbackUSDPerToken prices a hosted model with no spend history
	// yet: $3 per million tokens, a mid-range chat model.
	fallbackUSDPerToken = 3e-6
	// ocrUSDPerPage is mistral-ocr's price ($2 per 1,000 pages).
	ocrUSDPerPage = 0.002
	// webSearchUSD is OpenRouter's web search, about $0.02 a search, plus
	// the small model call around it.
	webSearchUSD = 0.02
	// pdfTokensPerPage is roughly how much text a page brings.
	pdfTokensPerPage = 600
	// replyTokens is a typical reply's length, for estimates.
	replyTokens = 400
	// estimateFloorUSD: below this, a turn isn't worth checking or
	// recording (the caps themselves still apply to every call).
	estimateFloorUSD = 0.01
	// costAskMaxAge is how long "go ahead" answers a cost question.
	costAskMaxAge = 15 * time.Minute
	// taskModelCalls is how many model calls a background task usually
	// takes (it may take up to maxModelCallsPerTurn).
	taskModelCalls = 3
)

// costHold stops a turn before its first model call: msg is said to the
// person, in code, instead of a reply.
type costHold struct {
	msg   string
	asked bool // a question ("go ahead?"), not a refusal
}

func (c *costHold) Error() string { return c.msg }

// costAsk is a turn waiting for "go ahead".
type costAsk struct {
	at         time.Time
	user       int64
	t          *turn
	estimateID int64
}

// askAbove is the estimate above which the person is asked first (0 =
// never asked).
func (e *Engine) askAbove(ctx context.Context) float64 {
	return e.Settings.Float(ctx, "budget.ask_above_usd")
}

// usdPerToken is what a token costs on a model: from what it has cost
// over the last month, else a typical price. A local model is free.
func (e *Engine) usdPerToken(ctx context.Context, model string) float64 {
	if e.localModel(ctx) {
		return 0
	}
	if model == "" {
		model = e.Settings.String(ctx, "model")
	}
	if r, ok, err := e.DB.ModelRate(ctx, model, e.now().AddDate(0, -1, 0)); err == nil && ok {
		return r
	}
	return fallbackUSDPerToken
}

// money formats a figure for chat.
func money(usd float64) string { return fmt.Sprintf("$%.2f", usd) }

// estimateTurn is what this turn's first model call (and its PDFs) will
// cost, and a few words on why.
func (e *Engine) estimateTurn(ctx context.Context, t *turn, msgs []llm.Message, model string) (float64, string) {
	rate := e.usdPerToken(ctx, model)
	tokens := llm.PromptChars(msgs)/4 + replyTokens
	pages := 0
	for _, n := range t.pdfPages {
		pages += n
	}
	tokens += pages * pdfTokensPerPage
	usd := float64(tokens) * rate
	var parts []string
	if pages > 0 {
		label := fmt.Sprintf("%d pages", pages)
		if len(t.pdfs) == 1 {
			label = fmt.Sprintf("%s (%d pages)", t.pdfs[0].Name, pages)
		}
		if e.Settings.String(ctx, "pdf_engine") == "mistral-ocr" {
			ocr := float64(pages) * ocrUSDPerPage
			usd += ocr
			parts = append(parts, fmt.Sprintf("reading %s with OCR, about %s", label, money(ocr)))
		} else {
			parts = append(parts, "reading "+label)
		}
	}
	if tokens > 20000 {
		parts = append(parts, fmt.Sprintf("about %dk tokens of conversation on %s", tokens/1000, orModel(model, e.Settings.String(ctx, "model"))))
	}
	what := strings.Join(parts, " and ")
	if what == "" {
		what = "this reply"
	}
	return usd, what
}

func orModel(m, def string) string {
	if m != "" {
		return m
	}
	return def
}

// overCap says why an estimate can't go ahead: it would cross a cap, the
// one returned, with what's left of it. "" when it fits. What was already
// spent is in the headroom, so usd is only what's still to come.
func (e *Engine) overCap(ctx context.Context, sprig *character.Character, usd float64) (string, *llm.BudgetError) {
	why, tight, _ := e.overCapLeft(ctx, sprig, usd)
	return why, tight
}

func (e *Engine) overCapLeft(ctx context.Context, sprig *character.Character, usd float64) (string, *llm.BudgetError, float64) {
	if e.Budget == nil {
		return "", nil, 0
	}
	left, tight, ok, err := e.Budget.Headroom(ctx, sprig.ID)
	if err != nil || !ok || usd <= left {
		return "", nil, 0
	}
	whose := "the household's " + tight.Period + " cap"
	if tight.Sprig != "" {
		whose = sprig.DisplayName() + "'s " + tight.Period + " cap"
	}
	return fmt.Sprintf("that would cost about %s, which is %s more than %s has left (%s of %s)", money(usd), money(usd-left), whose, money(left), money(tight.Limit)), tight, left
}

// checkTurnCost runs before a turn's first model call. A costHold error
// stops the turn: over a cap it refuses, above the ask threshold it asks
// (once the person said go ahead, t.costOK skips the question).
func (e *Engine) checkTurnCost(ctx context.Context, t *turn, msgs []llm.Message, model string) error {
	if t.consult != nil {
		return nil // part of another sprig's turn, which was checked
	}
	usd, what := e.estimateTurn(ctx, t, msgs, model)
	if usd < estimateFloorUSD && len(t.pdfs) == 0 {
		return nil
	}
	kind := "turn"
	if len(t.pdfs) > 0 {
		kind = "pdf"
	}
	est := store.Estimate{UserID: t.user.ID, Character: t.char.ID, Kind: kind, What: what, EstimateUSD: usd}
	if why, tight, left := e.overCapLeft(ctx, t.char, usd); why != "" {
		est.Outcome = store.EstimateRefused
		_, _ = e.DB.AddEstimate(ctx, est)
		e.logActivity(ctx, "warn", "budget", t.char.ID, t.user.ID, "refused a costly reply: "+why, what)
		if t.user.IsKid() && left <= 0 {
			// Nothing left at all: that's the cap reached, not a big
			// request (a small one wouldn't work either).
			return &costHold{msg: e.kidCapReply(ctx, t, tight)}
		}
		if t.user.IsKid() {
			e.tellParentsRefused(ctx, t.char, t.user, tight, what, why)
			return &costHold{msg: "That's too big a job for my allowance right now. Ask a grown-up if you need it!"}
		}
		return &costHold{msg: fmt.Sprintf("I didn't start on that: %s, %s. You can raise the cap, or ask me something smaller.", what, why)}
	}
	threshold := e.askAbove(ctx)
	if !t.costOK && threshold > 0 && usd > threshold && !t.jobMode() && t.chatRef != "" && !t.user.IsKid() {
		est.Outcome = store.EstimateAsked
		id, _ := e.DB.AddEstimate(ctx, est)
		e.costAsks.Store(t.chatRef, &costAsk{at: e.now(), user: t.user.ID, t: t, estimateID: id})
		return &costHold{asked: true, msg: fmt.Sprintf("Before I start: %s will cost about %s (more than the %s I ask about). Reply **go ahead** and I'll do it, or **no** to leave it.",
			capFirst(what), money(usd), money(threshold))}
	}
	if t.estimateID == 0 && (usd >= threshold/2 || len(t.pdfs) > 0) {
		t.estimateID, _ = e.DB.AddEstimate(ctx, est)
	}
	return nil
}

func capFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// answerCostAsk handles "go ahead" or "no" after a cost question in this
// chat. Anything else drops the question (it's answered by moving on).
func (e *Engine) answerCostAsk(ctx context.Context, in channel.Incoming, user *store.User) (bool, error) {
	v, ok := e.costAsks.Load(in.ChatRef())
	if !ok {
		return false, nil
	}
	ask := v.(*costAsk)
	if ask.user != user.ID {
		return false, nil
	}
	e.costAsks.Delete(in.ChatRef())
	if e.now().Sub(ask.at) > costAskMaxAge || len(in.Attachments) > 0 {
		return false, nil
	}
	switch {
	case approveRe.MatchString(in.Text):
		t := ask.t
		t.costOK, t.estimateID = true, ask.estimateID
		if ask.estimateID != 0 {
			_ = e.DB.SetEstimateOutcome(ctx, ask.estimateID, store.EstimateRan)
		}
		if t.conv != nil {
			_ = e.DB.AddMessage(ctx, t.conv.ID, "user", "", in.Text)
		}
		return true, e.respond(ctx, t, "The user agreed to the cost of this; go ahead with what they asked.")
	case denyRe.MatchString(in.Text):
		return true, e.send(ctx, in.ChatRef(), ask.t.char, "Okay, I've left it.")
	}
	return false, nil
}

// meterTurn measures what a turn cost, against its estimate.
func (e *Engine) meterTurn(ctx context.Context, t *turn) (context.Context, func()) {
	ctx, m := budget.WithMeter(ctx)
	return ctx, func() {
		if t.estimateID != 0 {
			_ = e.DB.FinishEstimate(context.WithoutCancel(ctx), t.estimateID, m.USD())
			t.estimateID = 0
		}
	}
}

// estimateTool is what one tool call is expected to cost (0 = not
// estimated).
// who is the sprig whose caps it counts against: the one doing the work.
func (e *Engine) estimateTool(ctx context.Context, t *turn, name string, args json.RawMessage) (usd float64, what string, who *character.Character) {
	switch name {
	case "web_search":
		if p, err := e.searchProvider(ctx); err == nil && p == "openrouter" {
			return webSearchUSD, "a web search", t.char
		}
	case "delegate_task":
		a, _ := decode[struct {
			To   string `json:"to"`
			Task string `json:"task"`
		}](args)
		target := t.char
		if a.To != "" {
			if c, err := e.findSprig(t, a.To); err == nil {
				target = c
			}
		}
		prompt := max(t.promptChars, 8000) / 4
		usd := float64(taskModelCalls*(prompt+replyTokens)) * e.usdPerToken(ctx, target.Model)
		if target.HasScope(character.ScopeWeb) {
			if p, err := e.searchProvider(ctx); err == nil && p == "openrouter" {
				usd += 3 * webSearchUSD // a research task searches a few times
			}
		}
		return usd, fmt.Sprintf("%s's background task (%s)", target.DisplayName(), truncate(a.Task, 80)), target
	}
	return 0, "", t.char
}

// costGate checks a tool call's estimate before it runs. ok is false when
// it was refused (over a cap) or the person is being asked; msg is then
// what the model is told.
func (e *Engine) costGate(ctx context.Context, t *turn, tool *Tool, args json.RawMessage) (msg string, ok bool) {
	usd, what, who := e.estimateTool(ctx, t, tool.Name, args)
	if usd <= 0 {
		return "", true
	}
	est := toolEstimate(t, tool, who, usd, what)
	if why, _ := e.overCap(ctx, who, usd); why != "" {
		est.Outcome = store.EstimateRefused
		_, _ = e.DB.AddEstimate(ctx, est)
		e.logActivity(ctx, "warn", "budget", who.ID, t.user.ID, "refused "+tool.Name+": "+why, what)
		return fmt.Sprintf("Error: %s was NOT run: %s. Tell the user plainly, with the figures.", what, why), false
	}
	t.toolUSD += usd
	threshold := e.askAbove(ctx)
	if threshold > 0 && t.toolUSD > threshold && !t.user.IsKid() {
		est.Outcome = store.EstimateAsked
		t.askEstimate, _ = e.DB.AddEstimate(ctx, est) // goes with the approval
		t.toolUSD -= usd                              // asked about separately; approved runs aren't counted again
		note := fmt.Sprintf("💸 This will cost about %s (%s).", money(usd), what)
		if t.toolUSD > 0 {
			note = fmt.Sprintf("💸 This will cost about %s (%s), on top of about %s already spent on this request.", money(usd), what, money(t.toolUSD))
		}
		return e.requestApprovalNote(ctx, t, tool, args, note), false
	}
	t.toolEstimate = e.recordToolEstimate(ctx, t, est)
	return "", true
}

// recordToolEstimate records a tool call's estimate, unless the call is
// part of a background task whose own estimate covers the whole task (it
// would then be counted twice in the accuracy figures).
func (e *Engine) recordToolEstimate(ctx context.Context, t *turn, est store.Estimate) int64 {
	if t.taskFrom != nil && t.estimateID != 0 {
		return 0
	}
	id, _ := e.DB.AddEstimate(ctx, est)
	return id
}

// toolEstimate is the estimate row for a tool call.
func toolEstimate(t *turn, tool *Tool, who *character.Character, usd float64, what string) store.Estimate {
	est := store.Estimate{UserID: t.user.ID, Character: who.ID, Kind: tool.Name, What: what, EstimateUSD: usd}
	switch tool.Name {
	case "delegate_task":
		est.Kind = "task"
	case "web_search":
		est.Kind = "search"
	}
	return est
}

// approvedCost checks a tool call the person already said yes to, right
// before it runs: never asked again, but refused if the caps have closed
// in since. It records the estimate, for the tool run to be measured
// against (t.toolEstimate).
func (e *Engine) approvedCost(ctx context.Context, t *turn, tool *Tool, args json.RawMessage) string {
	t.toolEstimate = 0
	usd, what, who := e.estimateTool(ctx, t, tool.Name, args)
	if usd <= 0 && t.approvedEstimate == 0 {
		return ""
	}
	est := toolEstimate(t, tool, who, usd, what)
	// The figure the person was shown when asked, carried by this very
	// approval: an approval turn knows less than the turn that asked (its
	// prompt size, for one), so its own estimate can come out lower.
	var askedID int64
	if id := t.approvedEstimate; id != 0 {
		if asked, err := e.DB.EstimateUSD(ctx, id); err == nil {
			askedID = id
			usd = max(usd, asked)
			est.EstimateUSD = usd
		}
	}
	if why, _ := e.overCap(ctx, who, usd); why != "" {
		est.Outcome = store.EstimateRefused
		if askedID != 0 {
			_ = e.DB.SetEstimateOutcome(ctx, askedID, store.EstimateRefused)
		} else {
			_, _ = e.DB.AddEstimate(ctx, est)
		}
		return "Error: it wasn't run after all: " + why + "."
	}
	if askedID != 0 {
		// The row the person was asked about, now measured.
		_ = e.DB.SetEstimateOutcome(ctx, askedID, store.EstimateRan)
		t.toolEstimate = askedID
		return ""
	}
	t.toolEstimate = e.recordToolEstimate(ctx, t, est)
	return ""
}

// resetsWhen says when a cap resets, in the person's timezone.
func resetsWhen(be *llm.BudgetError, u *store.User) string {
	if be == nil || be.Resets.IsZero() {
		return "it resets"
	}
	return be.Resets.In(u.Location()).Format("Monday 2 January")
}

// kidCapReply is what a kid hears when their sprig hits a cap, and the
// parents are told once per cap and period.
func (e *Engine) kidCapReply(ctx context.Context, t *turn, be *llm.BudgetError) string {
	if be.Period == "" {
		// Not a cap (spend can't be recorded right now): nothing for the
		// parents to raise.
		return "I can't help right now, sorry. Try again a bit later!"
	}
	when := resetsWhen(be, t.user)
	e.tellParentsCap(ctx, t.char, t.user, be, when)
	if be.Period == "daily" {
		return "I've used up my allowance for today, so I need a rest until tomorrow. A grown-up can give me more if it's important!"
	}
	return fmt.Sprintf("I've used up my allowance for this month, so I need a rest until %s. A grown-up can give me more if it's important!", when)
}

// tellParentsCap tells a kid sprig's parents (else the admins) that it hit
// a cap, once per cap and period.
func (e *Engine) tellParentsCap(ctx context.Context, c *character.Character, kid *store.User, be *llm.BudgetError, when string) {
	if be == nil || be.Period == "" {
		return
	}
	key := fmt.Sprintf("kidcap:%s:%s:%s", c.ID, be.Period, be.Resets.Format("2006-01-02"))
	if ok, err := e.DB.ClaimNotice(ctx, key, 40*24*time.Hour); err != nil || !ok {
		return
	}
	host, ok := e.Chars.Host()
	if !ok {
		return
	}
	capDesc := "the household's " + be.Period + " cap"
	how := "An admin can raise the household's cap."
	if be.Sprig != "" {
		capDesc = fmt.Sprintf("its %s cap of %s", be.Period, money(be.Limit))
		how = fmt.Sprintf("To give it more, tell me \"set %s's %s cap to $%s\" (only a parent can raise it).", c.DisplayName(), be.Period, strconv.FormatFloat(be.Limit*2, 'f', -1, 64))
	}
	text := fmt.Sprintf("%s has used %s, so it's resting until %s. I told %s plainly. %s", c.DisplayName(), capDesc, when, nameOr(kid.Name, "your kid"), how)
	var told []int64
	for _, id := range c.Parents {
		if p, err := e.DB.UserByID(ctx, id); err == nil {
			if err := e.sendNotice(ctx, p, host, text); err == nil {
				told = append(told, id)
			}
		}
	}
	if len(told) == 0 {
		admins, _ := e.DB.Admins(ctx)
		for _, a := range admins {
			_ = e.sendNotice(ctx, &a, host, text)
		}
	}
	e.logActivity(ctx, "warn", "budget", c.ID, kid.ID, fmt.Sprintf("%s hit %s; its parents were told", c.DisplayName(), capDesc), "")
}

// tellParentsRefused tells a kid sprig's parents (else the admins) that it
// turned down something too costly for what's left of a cap: unlike
// tellParentsCap, the cap isn't used up, and smaller things still work.
// Once a day per sprig, apart from the cap-reached notice.
func (e *Engine) tellParentsRefused(ctx context.Context, c *character.Character, kid *store.User, be *llm.BudgetError, what, why string) {
	if be == nil || be.Period == "" {
		return
	}
	key := fmt.Sprintf("kidrefused:%s:%s", c.ID, e.now().In(kid.Location()).Format("2006-01-02"))
	if ok, err := e.DB.ClaimNotice(ctx, key, 36*time.Hour); err != nil || !ok {
		return
	}
	host, ok := e.Chars.Host()
	if !ok {
		return
	}
	text := fmt.Sprintf("%s turned down something %s asked for (%s): %s. It still helps with smaller things.", c.DisplayName(), nameOr(kid.Name, "your kid"), what, why)
	if be.Sprig != "" {
		text += fmt.Sprintf(" To give it more, tell me \"set %s's %s cap to $%s\" (only a parent can raise it).", c.DisplayName(), be.Period, strconv.FormatFloat(be.Limit*2, 'f', -1, 64))
	}
	e.notifyParents(ctx, c, host, text)
	e.logActivity(ctx, "info", "budget", c.ID, kid.ID, c.DisplayName()+" turned down a costly request; its parents were told", "")
}

// notifyParents sends a notice to a kid sprig's parents, else the admins.
func (e *Engine) notifyParents(ctx context.Context, c *character.Character, host *character.Character, text string) {
	told := 0
	for _, id := range c.Parents {
		if p, err := e.DB.UserByID(ctx, id); err == nil && e.sendNotice(ctx, p, host, text) == nil {
			told++
		}
	}
	if told == 0 {
		admins, _ := e.DB.Admins(ctx)
		for _, a := range admins {
			_ = e.sendNotice(ctx, &a, host, text)
		}
	}
}

// budgetReply is what an adult hears when a cap stops a reply.
func budgetReply(t *turn, be *llm.BudgetError) string {
	if be == nil || be.Sprig == "" {
		return "I've hit the spending cap, so I'm going quiet to save your wallet. You can raise it by telling me a new daily or monthly cap."
	}
	when := ""
	if !be.Resets.IsZero() {
		when = " until " + be.Resets.In(t.user.Location()).Format("Monday 2 January")
	}
	return fmt.Sprintf("I've used my own %s spending cap (%s), so I'm resting%s. Ask Juni to raise %s's %s cap if you need me sooner.",
		be.Period, money(be.Limit), when, t.char.DisplayName(), be.Period)
}

// SetSprigCap sets one sprig's daily or monthly cap (usd 0 = no cap). A
// kid sprig's cap can only be raised (or removed) by its parents; anyone
// who manages a sprig may lower it. It goes in the trail, with undo.
func (e *Engine) SetSprigCap(ctx context.Context, by *store.User, sprig, period string, usd float64) (string, error) {
	e.capMu.Lock()
	defer e.capMu.Unlock()
	return e.setSprigCap(ctx, by, sprig, period, usd, true)
}

// setSprigCap is SetSprigCap with capMu held; record puts the change in
// the trail (an undo doesn't: its own entry says it).
func (e *Engine) setSprigCap(ctx context.Context, by *store.User, sprig, period string, usd float64, record bool) (string, error) {
	if period != "daily" && period != "monthly" {
		return "", errors.New("the cap is daily or monthly")
	}
	if math.IsNaN(usd) || math.IsInf(usd, 0) || usd < 0 {
		return "", errors.New("a cap must be a number of dollars, not negative (0 means no cap)")
	}
	c, ok := e.Chars.Get(sprig)
	if !ok {
		c, ok = e.Chars.FindByName(sprig)
	}
	if !ok || c.PrivateFrom(by.ID) {
		return "", errNoSuchSprig
	}
	name := settings.SprigCap(c.ID, period)
	prev := e.Settings.Float(ctx, name)
	raising := usd == 0 && prev > 0 || prev > 0 && usd > prev
	switch {
	case by.IsKid():
		return "", notAllowed("kids can't change spending caps")
	case c.IsKid() && raising && !c.IsParent(by.ID):
		e.refused(ctx, by, c.ID, fmt.Sprintf("tried to raise %s's %s cap", c.DisplayName(), period))
		return "", notAllowed(fmt.Sprintf("only %s's parents can raise its cap", c.DisplayName()))
	case c.IsKid() && (c.IsParent(by.ID) || by.IsAdmin()):
	case !canManage(by, c):
		return "", e.mayManage(ctx, by, c, "change a spending cap")
	}
	value := ""
	if usd > 0 {
		value = strconv.FormatFloat(usd, 'f', -1, 64)
	}
	if err := e.Settings.Set(ctx, name, value); err != nil {
		return "", err
	}
	if record && e.trailCap(ctx, by, c, period, prev, usd) == 0 {
		// Unrecorded, a later undo couldn't tell this was the newest
		// change: put the old cap back and fail instead.
		old := ""
		if prev > 0 {
			old = strconv.FormatFloat(prev, 'f', -1, 64)
		}
		_ = e.Settings.Set(context.WithoutCancel(ctx), name, old)
		return "", errors.New("couldn't record the change, so the cap wasn't changed")
	}
	return fmt.Sprintf("%s's %s cap is now %s.", c.DisplayName(), period, capText(usd)), nil
}

func capText(usd float64) string {
	if usd <= 0 {
		return "off (no cap of its own)"
	}
	return money(usd)
}

func (e *Engine) trailCap(ctx context.Context, by *store.User, c *character.Character, period string, prev, next float64) int64 {
	id := e.addTrail(ctx, userID(by), c.ID, store.TrailCap, fmt.Sprintf("%s's %s cap: %s → %s", c.DisplayName(), period, capText(prev), capText(next)), "",
		&trailUndo{Type: "cap", Character: c.ID, Tool: period, Level: strconv.FormatFloat(prev, 'f', -1, 64), Set: strconv.FormatFloat(next, 'f', -1, 64)})
	e.logActivity(ctx, "info", "budget", c.ID, userID(by), fmt.Sprintf("%s's %s cap set to %s", c.DisplayName(), period, capText(next)), "")
	return id
}

// presetCap stores a new sprig's monthly cap before its file is written,
// so it's never live without it (usd 0: none). undo takes it back if the
// sprig isn't created after all.
func (e *Engine) presetCap(ctx context.Context, id string, usd float64) (undo func(), err error) {
	if usd <= 0 {
		return func() {}, nil
	}
	name := settings.SprigCap(id, "monthly")
	if err := e.DB.SetSetting(ctx, name, strconv.FormatFloat(usd, 'f', -1, 64)); err != nil {
		return nil, fmt.Errorf("couldn't set its spend cap, so it wasn't created: %w", err)
	}
	return func() { _ = e.DB.DeleteSetting(context.WithoutCancel(ctx), name) }, nil
}

func (e *Engine) toolSetSprigCap(ctx context.Context, t *turn, args json.RawMessage) (string, error) {
	a, err := decode[struct {
		Sprig      string   `json:"sprig"`
		DailyUSD   *float64 `json:"daily_usd"`
		MonthlyUSD *float64 `json:"monthly_usd"`
	}](args)
	if err != nil {
		return "", err
	}
	if a.DailyUSD == nil && a.MonthlyUSD == nil {
		c, ok := e.Chars.Get(a.Sprig)
		if !ok {
			c, ok = e.Chars.FindByName(a.Sprig)
		}
		if !ok || c.PrivateFrom(t.user.ID) || c.IsKid() && !c.IsParent(t.user.ID) && !t.user.IsAdmin() {
			return "", errNoSuchSprig
		}
		s, err := e.Budget.SprigCapsOf(ctx, c.ID)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%s: daily cap %s (spent %s today), monthly cap %s (spent %s this month).", c.DisplayName(),
			capText(s.Daily), money(s.SpentToday), capText(s.Monthly), money(s.SpentMonth)), nil
	}
	var out []string
	for _, p := range []struct {
		period string
		v      *float64
	}{{"daily", a.DailyUSD}, {"monthly", a.MonthlyUSD}} {
		if p.v == nil {
			continue
		}
		s, err := e.SetSprigCap(ctx, t.user, a.Sprig, p.period, *p.v)
		if err != nil {
			return "", err
		}
		out = append(out, s)
	}
	return strings.Join(out, " "), nil
}

// fileExists reports whether a sprig's character file is on disk.
func (e *Engine) fileExists(id string) bool {
	_, err := os.Stat(filepath.Join(e.Chars.Dir(), id+".md"))
	return err == nil
}

// unpublish removes a new sprig's character file after a failed create,
// and reloads the registry so it isn't live. Only for a file the create
// itself wrote (existed is false): never someone's own file. It reports
// whether the file is gone; if not, the caller keeps the sprig's cap, so
// a file that may still load never loads uncapped. Once its Load returns,
// no reload that started before the removal can bring the sprig back, so
// the cap can go.
func (e *Engine) unpublish(id string, existed bool) bool {
	if existed {
		return false
	}
	path := filepath.Join(e.Chars.Dir(), id+".md")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		e.Log.Error("remove a half-created sprig; keeping its cap", "path", path, "err", err)
		return false
	}
	if err := e.Chars.Load(); err != nil {
		// The old snapshot, with the sprig in it, may still be live.
		e.Log.Error("reload sprigs after removing a half-created sprig; keeping its cap", "err", err)
		return false
	}
	return true
}
