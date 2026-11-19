package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/llm"
	"github.com/theshield2594/thicket/internal/store"
)

// Kid sprigs in chat: Juni interviews an admin (which kids, who the
// parents are, a name and personality), the new sprig says hello as it
// would to the kid, and create shows the parent preview and asks for
// approval. Only the approved spec is created, through CreateKidSprig,
// so the same checks, cap and trail apply as from the API. Afterwards the
// parents hear what they can do, and the kid gets a first hello.

// kidSprigApproval is what the admin approves: the spec, and the monthly
// cap the parent preview showed.
type kidSprigApproval struct {
	Spec   KidSprigSpec `json:"spec"`
	CapUSD float64      `json:"cap_usd"`
}

// kidDraft is an admin's latest kid sprig preview.
type kidDraft struct {
	spec                  KidSprigSpec
	kidNames, parentNames []string
}

type kidSprigArgs struct {
	Action      string   `json:"action"`
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Role        string   `json:"role"`
	Traits      []string `json:"traits"`
	VoiceStyle  []string `json:"voice_style"`
	Description string   `json:"description"`
	Kids        []string `json:"kids"`
	Parents     []string `json:"parents"`
	Color       string   `json:"color"`
}

const kidSprigSchema = `{"type":"object","properties":{` +
	`"action":{"type":"string","enum":["preview","create"],"description":"preview: the draft says hello as it would to the kid (call again with changes); create: show the parent preview of the latest draft and ask for approval"},` +
	`"id":{"type":"string","description":"short lowercase id, e.g. bramble"},"name":{"type":"string"},` +
	`"role":{"type":"string","description":"one sentence: who it is for the kid"},` +
	`"traits":{"type":"array","items":{"type":"string"}},"voice_style":{"type":"array","items":{"type":"string"}},` +
	`"description":{"type":"string","description":"a short background: what it likes, how it plays and talks"},` +
	`"kids":{"type":"array","items":{"type":"string"},"description":"the kids it's for, by name (default: the household's only kid)"},` +
	`"parents":{"type":"array","items":{"type":"string"},"description":"the adults who look after it, by name (default: you, the person setting it up)"},` +
	`"color":{"type":"string","description":"#rrggbb"}},"required":["action","id"]}`

func (e *Engine) toolCreateKidSprig(ctx context.Context, t *turn, args json.RawMessage) (string, error) {
	a, err := decode[kidSprigArgs](args)
	if err != nil {
		return "", err
	}
	if t.user.IsKid() || !t.user.IsAdmin() {
		return "", errors.New("only the owner or an admin can set up a kid sprig: ask them to")
	}
	// The preview and the parent preview name the household's kids and
	// parents: never in a group channel.
	if !t.isDM {
		return "A kid sprig is set up in a DM, never in a group channel: ask them to continue in a DM (or the web app) and set it up there.", nil
	}
	if a.Action == "create" {
		return e.proposeKidSprig(ctx, t, strings.ToLower(strings.TrimSpace(a.ID)))
	}
	return e.previewKidSprig(ctx, t, a)
}

// previewKidSprig checks the draft and has it say hello in the chat, as
// it would to the kid.
func (e *Engine) previewKidSprig(ctx context.Context, t *turn, a kidSprigArgs) (string, error) {
	kids, err := e.resolveKids(ctx, a.Kids)
	if err != nil {
		return "", err
	}
	parents, err := e.resolveParents(ctx, t.user, a.Parents)
	if err != nil {
		return "", err
	}
	color := strings.TrimSpace(a.Color)
	if color == "" {
		color = "#e0a458"
	}
	spec := KidSprigSpec{
		ID: strings.ToLower(strings.TrimSpace(a.ID)), Name: strings.TrimSpace(a.Name), Role: strings.TrimSpace(a.Role),
		Traits: a.Traits, VoiceStyle: a.VoiceStyle, Description: strings.TrimSpace(a.Description),
		Kids: kids, Parents: parents, Color: color,
	}
	if spec.Name != "" && e.sprigNameTaken(spec.Name) {
		return "", fmt.Errorf("there's already a sprig called %s; pick another name", spec.Name)
	}
	c, err := e.kidSprigFromSpec(ctx, t.user, spec)
	if err != nil {
		return "", errors.New("the draft needs fixing: " + err.Error())
	}
	d := &kidDraft{spec: spec}
	var firstKid *store.User
	for _, id := range kids {
		u, err := e.DB.UserByID(ctx, id)
		if err != nil {
			return "", err
		}
		if firstKid == nil {
			firstKid = u
		}
		d.kidNames = append(d.kidNames, userName(u))
	}
	for _, id := range parents {
		u, err := e.DB.UserByID(ctx, id)
		if err != nil {
			return "", err
		}
		d.parentNames = append(d.parentNames, userName(u))
	}
	intro, err := e.kidHelloText(ctx, c, firstKid, t.user.ID, "preview")
	if err != nil {
		return "", fmt.Errorf("couldn't generate the preview: %w", err)
	}
	preview := *c
	preview.Nickname = c.DisplayName() + " (preview)"
	if err := e.send(ctx, t.chatRef, &preview, intro); err != nil {
		return "", err
	}
	if t.conv != nil {
		_ = e.DB.AddMessage(ctx, t.conv.ID, "event", "", fmt.Sprintf("Preview of new kid sprig %s said hello as it would to %s: %s", c.DisplayName(), strings.Join(d.kidNames, " and "), intro))
	}
	e.kidDrafts.Store(t.user.ID, d)
	return fmt.Sprintf("The preview of %s has been posted in the chat, saying hello as it would to %s (it said: %q). Ask what they think; adjust with another preview, or create it once they're happy. Don't repeat the hello.",
		c.DisplayName(), strings.Join(d.kidNames, " and "), intro), nil
}

// proposeKidSprig shows the parent preview of the latest draft and asks
// for approval. Nothing is created until then.
func (e *Engine) proposeKidSprig(ctx context.Context, t *turn, id string) (string, error) {
	v, ok := e.kidDrafts.Load(t.user.ID)
	if !ok {
		return "", errors.New("there's no kid sprig preview to create; call this with action preview first so they can see it")
	}
	d := v.(*kidDraft)
	if d.spec.ID != id {
		return "", fmt.Errorf("the latest kid sprig preview is %q, not %q", d.spec.ID, id)
	}
	c, err := e.kidSprigFromSpec(ctx, t.user, d.spec)
	if err != nil {
		return "", err
	}
	// The cap is read once: the preview shows it and creation installs it.
	capUSD := e.Settings.Float(ctx, "budget.kid_monthly_usd")
	b, err := json.Marshal(kidSprigApproval{Spec: d.spec, CapUSD: capUSD})
	if err != nil {
		return "", err
	}
	t.askAs(t.char, e.kidSprigPreview(c, d, capUSD), &store.Approval{UserID: t.user.ID, Character: t.char.ID, ChatRef: t.chatRef,
		Tool: internalPrefix + "create_kid_sprig", Args: string(b)})
	e.logActivity(ctx, "info", "character", t.char.ID, t.user.ID, "proposed kid sprig "+c.DisplayName(), "")
	return fmt.Sprintf("PENDING: the user is being shown the parent preview of %s and asked to approve. Tell them briefly to look it over; don't say it's created.", c.DisplayName()), nil
}

// kidSprigPreview is the parent preview: who the sprig will be and every
// limit it has.
func (e *Engine) kidSprigPreview(c *character.Character, d *kidDraft, capUSD float64) string {
	var b strings.Builder
	kids, parents := strings.Join(d.kidNames, " and "), strings.Join(d.parentNames, " and ")
	fmt.Fprintf(&b, "🌱 Parent preview: here's who %s will be for %s. Nothing is created until you say so.\n", c.DisplayName(), kids)
	fmt.Fprintf(&b, "- **%s** (`%s`): %s\n", c.DisplayName(), c.ID, c.Role)
	fmt.Fprintf(&b, "- Personality: %s. Voice: %s.\n", strings.Join(c.Personality.Traits, ", "), strings.Join(c.Voice.Style, "; "))
	if d.spec.Description != "" {
		fmt.Fprintf(&b, "- Background: %s\n", oneLine(d.spec.Description, 400))
	}
	fmt.Fprintf(&b, "- Talks only to %s. No adult can chat with it, Juni included, and it never messages anyone on its own.\n", kids)
	fmt.Fprintf(&b, "- Parents: %s. They can read every chat, decide what it may use, and get a safety alert if something worrying comes up.\n", parents)
	b.WriteString("- Starts with no tools, no family memory and no calendar: only the kid's own memories. Parents can allow more, one thing at a time.\n")
	if capUSD > 0 {
		fmt.Fprintf(&b, "- Spending: at most **%s a month**, which only its parents can raise.\n", money(capUSD))
	}
	fmt.Fprintf(&b, "Once it's created, its parents hear what they can do, and %s gets a first hello.\nReply **approve** or **deny**.", kids)
	return b.String()
}

// createKidSprigApproved creates an approved kid sprig, exactly as shown,
// then tells its parents what they can do and has it say hello to the kid.
func (e *Engine) createKidSprigApproved(ctx context.Context, user *store.User, args json.RawMessage) (string, error) {
	var ap kidSprigApproval
	if err := json.Unmarshal(args, &ap); err != nil {
		return "", err
	}
	spec := ap.Spec
	// Exactly the cap the parent preview showed, whatever the setting is now.
	c, err := e.createKidSprig(ctx, user, spec, ap.CapUSD)
	if err != nil {
		return "", err
	}
	// Drop the draft only if it's still the one approved: a newer preview stays.
	if v, ok := e.kidDrafts.Load(user.ID); ok && reflect.DeepEqual(v.(*kidDraft).spec, spec) {
		e.kidDrafts.CompareAndDelete(user.ID, v)
	}
	bg := context.WithoutCancel(ctx)
	var kidNames, parentNames []string
	var kids []*store.User
	for _, id := range c.Kids {
		if u, err := e.DB.UserByID(bg, id); err == nil {
			kids = append(kids, u)
			kidNames = append(kidNames, userName(u))
		}
	}
	for _, id := range c.Parents {
		if u, err := e.DB.UserByID(bg, id); err == nil {
			parentNames = append(parentNames, userName(u))
		}
	}
	kidList := strings.Join(kidNames, " and ")
	guide := kidParentGuide(c)
	// The other parents hear it from Juni; the one who approved it hears it
	// in this reply.
	host, _ := e.Chars.Host()
	var unreached []string
	for _, pid := range c.Parents {
		if pid == user.ID {
			continue
		}
		parent, err := e.DB.UserByID(bg, pid)
		if err != nil || parent.IsKid() {
			unreached = append(unreached, fmt.Sprintf("user %d", pid))
			continue
		}
		msg := fmt.Sprintf("🌱 %s set up %s, a kid sprig for %s, with you as one of its parents.\n%s", userName(user), c.DisplayName(), kidList, guide)
		if host == nil {
			err = errors.New("no host to send it")
		} else {
			err = e.sendNotice(bg, parent, host, msg)
		}
		if err != nil {
			e.Log.Warn("tell a parent about a new kid sprig", "parent", pid, "err", err)
			unreached = append(unreached, userName(parent))
		}
	}
	hellos := 0
	for _, kid := range kids {
		if err := e.kidFirstHello(bg, c, kid, user.ID); err != nil {
			e.Log.Warn("kid sprig's first hello", "kid", kid.ID, "err", err)
			e.logActivity(bg, "warn", "character", c.ID, kid.ID, "first hello not delivered", shortErr(err))
			continue
		}
		hellos++
	}
	out := fmt.Sprintf("%s has been created for %s. Only they can talk to it; %s can read its chats. Tell the user (they're a parent) what they can do:\n%s",
		c.DisplayName(), kidList, strings.Join(parentNames, " and "), guide)
	if !slices.Contains(c.Parents, user.ID) {
		out = fmt.Sprintf("%s has been created for %s. Its parents (%s) have been told what they can do; the user approved it but isn't one of its parents, so they can't read its chats.",
			c.DisplayName(), kidList, strings.Join(parentNames, " and "))
		if len(unreached) > 0 {
			out = fmt.Sprintf("%s has been created for %s, with %s as its parents; the user approved it but isn't one of them, so they can't read its chats.",
				c.DisplayName(), kidList, strings.Join(parentNames, " and "))
		}
	}
	if len(unreached) > 0 {
		out += fmt.Sprintf("\nThe note telling %s what they can do couldn't be delivered: tell them, or ask them to look under Household in the web app.", strings.Join(unreached, " and "))
	}
	switch {
	case hellos == len(kids) && hellos > 0:
		out += fmt.Sprintf("\n%s has said a first hello to %s.", c.DisplayName(), kidList)
	case len(kids) > 0:
		out += fmt.Sprintf("\nThe first hello to %s couldn't be delivered everywhere; %s will say hello the first time they write.", kidList, c.DisplayName())
	}
	return out, nil
}

// kidParentGuide is what a kid sprig's parents can do.
func kidParentGuide(c *character.Character) string {
	return fmt.Sprintf("- Read the whole chat in the web app: Household → \"%s's chats\".\n"+
		"- Approve what it may use, one tool at a time, on that same screen. It starts with none.\n"+
		"- Allow family memory or the calendar there too, if you'd like it to know them.\n"+
		"- If a chat touches something worrying (self-harm, a stranger asking for personal details, bullying), a safety alert comes to you here in chat.",
		c.DisplayName())
}

// kidFirstHello has a new kid sprig say hello to a kid, then counts the
// kid as greeted so it isn't repeated the first time they write.
func (e *Engine) kidFirstHello(ctx context.Context, c *character.Character, kid *store.User, by int64) error {
	text, err := e.kidHelloText(ctx, c, kid, by, "hello")
	if err != nil {
		return err
	}
	if err := e.sendNotice(ctx, kid, c, text); err != nil {
		return err
	}
	if kid.OnboardedAt.IsZero() {
		if err := e.DB.MarkOnboarded(ctx, kid.ID); err == nil {
			kid.OnboardedAt = e.now()
		}
	}
	return nil
}

// kidHelloText is a kid sprig's first hello to a kid, in its own voice,
// with the prompt it will have when the kid writes.
func (e *Engine) kidHelloText(ctx context.Context, c *character.Character, kid *store.User, by int64, purpose string) (string, error) {
	sys := buildSystemPrompt(promptInput{char: c, user: kid, roster: []*character.Character{c}, now: e.now(), onboarded: false, isDM: true})
	resp, err := e.LLM.Chat(llm.WithMeta(ctx, llm.Meta{UserID: by, Character: c.ID, Purpose: purpose}), llm.Request{
		Messages: []llm.Message{
			{Role: "system", Content: sys},
			{Role: "user", Content: fmt.Sprintf("[Thicket event] You were just set up for %s and are saying hello for the very first time. Say hello in two or three short sentences, fully in your own voice.", userName(kid))},
		},
		MaxTokens: 300,
	})
	if err != nil {
		return "", err
	}
	text := cleanReply(resp.Message.Content, c)
	if strings.TrimSpace(text) == "" {
		return "", errors.New("the model said nothing")
	}
	return text, nil
}

// resolveParents turns names or ids into the household's adults. With
// none named, it's the person setting the sprig up.
func (e *Engine) resolveParents(ctx context.Context, by *store.User, names []string) ([]int64, error) {
	if len(names) == 0 {
		return []int64{by.ID}, nil
	}
	users, err := e.DB.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	var out []int64
	for _, n := range names {
		n = strings.TrimSpace(n)
		if strings.EqualFold(n, "me") {
			n = strconv.FormatInt(by.ID, 10)
		}
		found := false
		for _, u := range users {
			if !strings.EqualFold(u.Name, n) && strconv.FormatInt(u.ID, 10) != n {
				continue
			}
			if u.IsKid() {
				return nil, fmt.Errorf("%s is a kid, so can't be a parent of a kid sprig", userName(&u))
			}
			if !slices.Contains(out, u.ID) {
				out = append(out, u.ID)
			}
			found = true
			break
		}
		if !found {
			return nil, fmt.Errorf("%q isn't an adult in the household; invite them first, or leave them out", n)
		}
	}
	return out, nil
}
