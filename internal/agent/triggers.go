package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/theshield2594/thicket/internal/store"
)

// Event triggers: routines that fire on something happening instead of
// the clock. Thicket has no integrations of its own, so events come from
// what's already plugged in:
//
//   - an integration (MCP) tool that only reads, polled every few minutes:
//     the job fires with the items that are new since the last poll and
//     match the filter ("new email from X", "calendar event added",
//     "container unhealthy" from a Docker or monitoring integration);
//   - a webhook URL, as before, optionally filtered by what was sent.
//
// The first poll only records what's there, so nothing old fires.

// Matches reports whether text matches a trigger filter: "" matches
// everything, /regex/ is a case-insensitive regular expression, anything
// else a case-insensitive substring.
func Matches(filter, text string) bool {
	if filter == "" {
		return true
	}
	if re := filterRegexp(filter); re != nil {
		return re.MatchString(text)
	}
	return strings.Contains(strings.ToLower(text), strings.ToLower(filter))
}

func filterRegexp(filter string) *regexp.Regexp {
	if len(filter) < 2 || filter[0] != '/' || filter[len(filter)-1] != '/' {
		return nil
	}
	re, err := regexp.Compile("(?i)" + filter[1:len(filter)-1])
	if err != nil {
		return nil
	}
	return re
}

func checkFilter(filter string) error {
	if len(filter) > 300 {
		return errors.New("the filter is too long (300 characters max)")
	}
	if len(filter) >= 2 && filter[0] == '/' && filter[len(filter)-1] == '/' {
		if _, err := regexp.Compile("(?i)" + filter[1:len(filter)-1]); err != nil {
			return fmt.Errorf("the filter isn't a valid regular expression: %v", err)
		}
	}
	return nil
}

// Trigger poll limits.
const (
	minPollMinutes   = 5
	maxItemsPerFire  = 20
	maxItemChars     = 1000
	pollCallTimeout  = 60 * time.Second
	triggerPollEvery = time.Minute
)

type item struct {
	key, text string
}

// splitItems turns a tool's output into items with stable keys: elements
// of a JSON array (or of the first array inside a JSON object), else
// non-empty lines. An element's "id" (or uid, message_id...) is its key;
// otherwise a hash of its text.
func splitItems(out string) []item {
	var raw []json.RawMessage
	var v any
	if json.Unmarshal([]byte(out), &v) == nil {
		switch x := v.(type) {
		case []any:
			_ = json.Unmarshal([]byte(out), &raw)
		case map[string]any:
			var obj map[string]json.RawMessage
			_ = json.Unmarshal([]byte(out), &obj)
			for _, k := range sortedKeys(x) {
				if _, ok := x[k].([]any); ok {
					_ = json.Unmarshal(obj[k], &raw)
					break
				}
			}
			if raw == nil {
				raw = []json.RawMessage{json.RawMessage(out)}
			}
		}
	}
	var items []item
	if raw != nil {
		for _, r := range raw {
			text := strings.TrimSpace(string(r))
			items = append(items, item{key: itemKey(r, text), text: text})
		}
		return items
	}
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			items = append(items, item{key: hashKey(line), text: line})
		}
	}
	return items
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func itemKey(r json.RawMessage, text string) string {
	var obj map[string]any
	if json.Unmarshal(r, &obj) == nil {
		for _, k := range []string{"id", "uid", "message_id", "messageId", "event_id", "uuid"} {
			if v, ok := obj[k]; ok && v != nil {
				return "id:" + fmt.Sprint(v)
			}
		}
	}
	return hashKey(text)
}

func hashKey(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "h:" + hex.EncodeToString(sum[:8])
}

// RunTriggers polls integration triggers until ctx ends.
func (e *Engine) RunTriggers(ctx context.Context) {
	t := time.NewTicker(triggerPollEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.PollTriggers(ctx)
		}
	}
}

// PollTriggers polls every integration trigger that is due, and fires
// its job with the new matching items. Exposed for tests.
func (e *Engine) PollTriggers(ctx context.Context) {
	if e.MCP == nil || e.Fire == nil {
		return
	}
	trigs, err := e.DB.PollTriggers(ctx)
	if err != nil {
		e.Log.Error("list triggers", "err", err)
		return
	}
	for _, tr := range trigs {
		every := time.Duration(max(tr.EveryMin, minPollMinutes)) * time.Minute
		if !tr.LastPollAt.IsZero() && e.now().Sub(tr.LastPollAt) < every-5*time.Second {
			continue
		}
		e.pollOne(ctx, tr)
	}
}

func (e *Engine) pollOne(ctx context.Context, tr store.Trigger) {
	j, err := e.DB.JobByID(ctx, tr.JobID)
	if err != nil {
		return
	}
	// At a spend cap, wait: a fire couldn't run, and items seen now would
	// be lost. They're picked up once heartbeats resume.
	if e.Budget.HeartbeatPause(ctx, j.Character) != "" {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, pollCallTimeout)
	res, err := e.MCP.Call(cctx, tr.Server, tr.Tool, json.RawMessage(tr.Args), true)
	cancel()
	if err == nil && res.IsError {
		err = errors.New(res.Text)
	}
	if err != nil {
		msg := e.Secrets.Redact(shortErr(err))
		_ = e.DB.SavePollError(ctx, tr.JobID, msg)
		e.deadLetter(ctx, store.InboxItem{UserID: j.UserID, Character: j.Character, Kind: store.InboxFailed, Ref: fmt.Sprintf("job:%d", j.ID),
			Title: fmt.Sprintf("Trigger for “%s” can't read %s → %s", j.Name, tr.Server, tr.Tool), Detail: msg})
		return
	}
	items := splitItems(e.Secrets.Redact(res.Text))
	seen := map[string]bool{}
	for _, k := range tr.Seen {
		seen[k] = true
	}
	keys := append([]string(nil), tr.Seen...)
	if !tr.Primed {
		// First look: remember what's already there, fire on nothing.
		for _, it := range items {
			if !seen[it.key] {
				seen[it.key] = true
				keys = append(keys, it.key)
			}
		}
		_ = e.DB.SavePoll(ctx, tr.JobID, keys, "")
		return
	}
	var fresh, matched []item
	for _, it := range items {
		if seen[it.key] {
			continue
		}
		seen[it.key] = true
		fresh = append(fresh, it)
		if Matches(tr.Match, it.text) {
			matched = append(matched, it)
		}
	}
	errText := ""
	fired := len(matched) == 0
	if len(matched) > 0 {
		jj := *j
		jj.Event = tr.Server + " → " + tr.Tool
		jj.Payload = eventPayload(matched)
		if err := e.Fire(jj); err != nil {
			errText = "new items found, but the job couldn't start: " + err.Error()
		} else {
			fired = true
		}
	}
	for _, it := range fresh {
		// Matched items that didn't fire are tried again next poll.
		if fired || !Matches(tr.Match, it.text) {
			keys = append(keys, it.key)
		}
	}
	_ = e.DB.SavePoll(ctx, tr.JobID, keys, errText)
}

func eventPayload(items []item) string {
	var b strings.Builder
	n := len(items)
	if n > maxItemsPerFire {
		items = items[len(items)-maxItemsPerFire:]
	}
	fmt.Fprintf(&b, "%d new item(s) since the last check:\n", n)
	for _, it := range items {
		text := it.text
		if len(text) > maxItemChars {
			text = text[:maxItemChars] + "…"
		}
		b.WriteString("- " + text + "\n")
	}
	if n > len(items) {
		fmt.Fprintf(&b, "(%d older ones not shown)\n", n-len(items))
	}
	return b.String()
}

func (e *Engine) toolCreateTrigger(ctx context.Context, t *turn, args json.RawMessage) (string, error) {
	a, err := decode[struct {
		Name   string          `json:"name"`
		Task   string          `json:"task"`
		Server string          `json:"integration"`
		Tool   string          `json:"tool"`
		Args   json.RawMessage `json:"args"`
		Every  int             `json:"every_minutes"`
		Match  string          `json:"match"`
		Owner  string          `json:"owner"`
	}](args)
	if err != nil {
		return "", err
	}
	owner := t.char
	if a.Owner != "" && a.Owner != t.char.ID {
		if owner, err = e.findSprig(a.Owner); err != nil {
			return "", err
		}
	}
	if strings.TrimSpace(a.Name) == "" || strings.TrimSpace(a.Task) == "" || len(a.Task) > 2000 {
		return "", errors.New("name and task are required (task max 2000 characters)")
	}
	if e.MCP == nil {
		return "", errors.New("no integrations are set up")
	}
	found := false
	for _, st := range e.MCP.Tools() {
		if st.Server == a.Server && st.Tool.Name == a.Tool {
			if !st.Tool.ReadOnly() {
				return "", fmt.Errorf("%s → %s changes things; a trigger can only watch a tool that only reads", a.Server, a.Tool)
			}
			found = true
		}
	}
	if !found {
		return "", fmt.Errorf("there's no connected integration tool %s → %s", a.Server, a.Tool)
	}
	if !owner.AllowsMCP(a.Server, a.Tool) {
		return "", fmt.Errorf("%s can't use %s → %s; grant it first", owner.DisplayName(), a.Server, a.Tool)
	}
	if err := checkFilter(a.Match); err != nil {
		return "", err
	}
	every := a.Every
	if every == 0 {
		every = 15
	}
	if every < minPollMinutes || every > 1440 {
		return "", fmt.Errorf("every_minutes must be between %d and 1440", minPollMinutes)
	}
	callArgs := "{}"
	if len(a.Args) > 0 && string(a.Args) != "null" {
		if !json.Valid(a.Args) || a.Args[0] != '{' {
			return "", errors.New("args must be a JSON object")
		}
		callArgs = string(a.Args)
	}
	j, err := e.DB.CreateJob(ctx, store.Job{UserID: t.user.ID, Character: owner.ID, Name: a.Name, Prompt: a.Task, Kind: store.JobWebhook})
	if err != nil {
		return "", err
	}
	if err := e.DB.PutTrigger(ctx, store.Trigger{JobID: j.ID, Source: store.TriggerMCP, Server: a.Server, Tool: a.Tool, Args: callArgs, EveryMin: every, Match: a.Match}); err != nil {
		_ = e.DB.DeleteJob(context.WithoutCancel(ctx), j.ID)
		return "", err
	}
	e.logActivity(ctx, "info", "job", owner.ID, t.user.ID, fmt.Sprintf("trigger job #%d %q created: watches %s → %s every %d min", j.ID, j.Name, a.Server, a.Tool, every), a.Match)
	filter := "every new item"
	if a.Match != "" {
		filter = fmt.Sprintf("new items matching %q", a.Match)
	}
	return fmt.Sprintf("Trigger job #%d created, run by %s: it checks %s → %s every %d minutes and runs the task with %s. "+
		"The first check only notes what's already there.", j.ID, owner.DisplayName(), a.Server, a.Tool, every, filter), nil
}

// describeTrigger is a trigger in a job list.
func describeTrigger(tr *store.Trigger) string {
	if tr.Source == store.TriggerWebhook {
		return fmt.Sprintf("webhook, only when the body matches %q", tr.Match)
	}
	s := fmt.Sprintf("on new items from %s → %s (checked every %d min)", tr.Server, tr.Tool, tr.EveryMin)
	if tr.Match != "" {
		s += fmt.Sprintf(" matching %q", tr.Match)
	}
	if tr.LastError != "" {
		s += "; last check: " + tr.LastError
	}
	return s
}
