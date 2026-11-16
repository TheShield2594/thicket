package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/llm"
	"github.com/theshield2594/thicket/internal/memory"
	"github.com/theshield2594/thicket/internal/scheduler"
	"github.com/theshield2594/thicket/internal/secrets"
	"github.com/theshield2594/thicket/internal/store"
)

// Tool is one capability offered to the model.
type Tool struct {
	Name        string
	Description string
	Params      string // JSON schema
	// Scope: "" = every character; "host" = the host only; a character
	// scope (schedule, characters, admin); or "mcp:<server>".
	Scope     string
	AdminOnly bool // the user must be an owner/admin
	JobOnly   bool
	ChatOnly  bool
	Risky     bool // needs approval unless a policy says otherwise
	Untrusted bool // output is outside content: wrap it as data
	Run       func(ctx context.Context, t *turn, args json.RawMessage) (string, error)
}

// Label is shown in approval prompts.
func (t *Tool) Label() string {
	if strings.HasPrefix(t.Scope, "mcp:") {
		return strings.TrimPrefix(t.Scope, "mcp:") + " → " + t.Description
	}
	return t.Name
}

func toolDefs(tools map[string]*Tool) []llm.ToolDef {
	names := make([]string, 0, len(tools))
	for n := range tools {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]llm.ToolDef, 0, len(names))
	for _, n := range names {
		t := tools[n]
		out = append(out, llm.ToolDef{Type: "function", Function: llm.FunctionDef{Name: t.Name, Description: t.Description, Parameters: json.RawMessage(t.Params)}})
	}
	return out
}

// toolsFor returns the tools this character may use in this turn.
// Permissions are decided here, in code.
func (e *Engine) toolsFor(ctx context.Context, t *turn) map[string]*Tool {
	out := map[string]*Tool{}
	for _, tool := range e.builtinTools() {
		if tool.JobOnly && !t.jobMode() || tool.ChatOnly && t.jobMode() {
			continue
		}
		// A sprig answering another sprig gets its own tools, minus the
		// conversation-shaping ones and the ability to pull in others.
		if t.consult != nil && (teamTools[tool.Name] || tool.ChatOnly || tool.JobOnly) {
			continue
		}
		if tool.AdminOnly && !t.user.IsAdmin() {
			continue
		}
		switch tool.Scope {
		case "":
		case "host":
			if !t.char.Host {
				continue
			}
		default:
			if !t.char.HasScope(tool.Scope) {
				continue
			}
		}
		out[tool.Name] = tool
	}
	if e.MCP != nil {
		for _, st := range e.MCP.Tools() {
			if !t.char.AllowsMCP(st.Server, st.Tool.Name) {
				continue
			}
			st := st
			params := string(st.Tool.InputSchema)
			if params == "" || params == "null" {
				params = `{"type":"object","properties":{}}`
			}
			desc := st.Tool.Description
			if len(desc) > 900 {
				desc = desc[:900]
			}
			out[st.FuncName()] = &Tool{
				Name:        st.FuncName(),
				Description: fmt.Sprintf("[%s integration] %s", st.Server, desc),
				Params:      params,
				Scope:       "mcp:" + st.Server,
				Risky:       !st.Tool.ReadOnly(),
				Untrusted:   true,
				Run: func(ctx context.Context, t *turn, args json.RawMessage) (string, error) {
					res, err := e.MCP.Call(ctx, st.Server, st.Tool.Name, args, st.Tool.ReadOnly())
					if err != nil {
						return "", err
					}
					if res.IsError {
						return "", errors.New(res.Text)
					}
					if res.Text == "" {
						return "(no output)", nil
					}
					return res.Text, nil
				},
			}
		}
	}
	return out
}

func decode[T any](args json.RawMessage) (T, error) {
	var v T
	if err := json.Unmarshal(args, &v); err != nil {
		return v, fmt.Errorf("bad arguments: %v", err)
	}
	return v, nil
}

func (e *Engine) builtinTools() []*Tool {
	return []*Tool{
		// --- memory (every sprig has its own memory space) ---
		{
			Name:        "remember",
			Description: "Save something worth remembering about the user. scope 'shared' = every sprig may know it (name, family, preferences); 'mine' = just between you and the user (in-jokes, your tasks). Set user_asked=true when the user explicitly asked you to remember it: those memories are locked and never changed automatically.",
			Params:      `{"type":"object","properties":{"content":{"type":"string","description":"one short third-person sentence"},"scope":{"type":"string","enum":["shared","mine"]},"kind":{"type":"string","enum":["fact","preference","person","project","inside_joke","shared_history","context"]},"user_asked":{"type":"boolean"}},"required":["content","scope"]}`,
			Run:         e.toolRemember,
		},
		{
			Name:        "list_memories",
			Description: "List everything you remember about the user (shared memories and your own). Use when they ask what you remember.",
			Params:      `{"type":"object","properties":{}}`,
			Run:         e.toolListMemories,
		},
		{
			Name:        "forget",
			Description: "Permanently delete memories by id (from list_memories or your prompt). Use when the user says 'forget that'.",
			Params:      `{"type":"object","properties":{"ids":{"type":"array","items":{"type":"integer"}}},"required":["ids"]}`,
			Run:         e.toolForget,
		},
		{
			Name:        "set_followup",
			Description: "Note something to check back with the user about later (e.g. 'how did the interview go?'). You'll be prompted to follow up when it's due.",
			Params:      `{"type":"object","properties":{"about":{"type":"string"},"due":{"type":"string","description":"YYYY-MM-DD or YYYY-MM-DDTHH:MM in the user's local time"}},"required":["about","due"]}`,
			Run:         e.toolFollowup,
		},
		{
			Name:        "handoff",
			Description: "Hand the conversation to another sprig whose job this is. They'll reply next.",
			Params:      `{"type":"object","properties":{"to":{"type":"string","description":"character id"},"message":{"type":"string","description":"what they should pick up"}},"required":["to","message"]}`,
			ChatOnly:    true,
			Run:         e.toolHandoff,
		},
		{
			Name:        "ask_sprig",
			Description: "Ask a fellow sprig something you need (what's on the calendar, what they know about a topic) and get their answer back, without handing the user over. Include the context they need: they don't see this conversation.",
			Params:      `{"type":"object","properties":{"to":{"type":"string","description":"sprig id or name"},"question":{"type":"string"}},"required":["to","question"]}`,
			Run:         e.toolAskSprig,
		},
		{
			Name:        "delegate_task",
			Description: "Give a fellow sprig (or yourself, with no 'to') a longer piece of work to do in the background, so the chat stays free. They message the user with the result when done. Write the task so it stands alone: they don't see this conversation.",
			Params:      `{"type":"object","properties":{"to":{"type":"string","description":"sprig id or name (default: you)"},"task":{"type":"string","description":"what to do and what to report"}},"required":["task"]}`,
			ChatOnly:    true,
			Run:         e.toolDelegateTask,
		},
		{
			Name:        "nothing_to_report",
			Description: "Call this in a scheduled job when there is genuinely nothing worth telling the user. The run is recorded as 'nothing found', not as a failure.",
			Params:      `{"type":"object","properties":{"reason":{"type":"string"}},"required":["reason"]}`,
			JobOnly:     true,
			Run: func(ctx context.Context, t *turn, args json.RawMessage) (string, error) {
				t.nothingToReport = true
				return "Recorded: nothing to report. You can stop now.", nil
			},
		},
		// --- host: onboarding ---
		{
			Name:        "set_user_profile",
			Description: "Save the user's name and/or IANA timezone (e.g. Europe/London, America/Chicago).",
			Params:      `{"type":"object","properties":{"name":{"type":"string"},"timezone":{"type":"string"}}}`,
			Scope:       "host",
			Run:         e.toolSetProfile,
		},
		{
			Name:        "complete_onboarding",
			Description: "Mark first-run onboarding as finished for this user.",
			Params:      `{"type":"object","properties":{}}`,
			Scope:       "host",
			ChatOnly:    true,
			Run: func(ctx context.Context, t *turn, _ json.RawMessage) (string, error) {
				if err := e.DB.MarkOnboarded(ctx, t.user.ID); err != nil {
					return "", err
				}
				t.user.OnboardedAt = e.now()
				return "Onboarding complete.", nil
			},
		},
		{
			Name:        "needs_you",
			Description: "The user's 'needs you' inbox: failed or interrupted jobs and tasks, results that couldn't be delivered, drafted actions, and approvals still waiting. action 'list' (default) shows them; 'resolve' closes one by id once the user has dealt with it.",
			Params:      `{"type":"object","properties":{"action":{"type":"string","enum":["list","resolve"]},"id":{"type":"integer"}}}`,
			Scope:       "host",
			Run:         e.toolNeedsYou,
		},
		// --- schedule ---
		{
			Name:        "schedule_job",
			Description: "Create a recurring job, e.g. a morning summary. schedule is cron (minute hour day month weekday) in the user's timezone, e.g. '30 7 * * 1-5' for weekdays at 7:30. Minimum interval 15 minutes. The owner sprig does the task and messages the user.",
			Params:      `{"type":"object","properties":{"name":{"type":"string"},"schedule":{"type":"string"},"task":{"type":"string","description":"instructions for the sprig each time it runs"},"owner":{"type":"string","description":"character id that runs it (default: you)"}},"required":["name","schedule","task"]}`,
			Scope:       "schedule",
			ChatOnly:    true,
			Run:         e.toolScheduleJob,
		},
		{
			Name:        "create_webhook",
			Description: "Create a job that runs when something outside Thicket calls a private URL (Home Assistant, CI, a script, another service), instead of on a schedule. Whatever the caller POSTs is given to the owner sprig as input. Thicket DMs the URL to the user; you never see it.",
			Params:      `{"type":"object","properties":{"name":{"type":"string"},"task":{"type":"string","description":"instructions for the sprig each time it's called, e.g. 'tell me what the CI failure was about'"},"owner":{"type":"string","description":"character id that runs it (default: you)"},"signature":{"type":"string","enum":["none","github","gitea","gitlab"],"description":"require calls signed the way that service signs webhooks (recommended when the sender is GitHub, Gitea/Forgejo or GitLab)"}},"required":["name","task"]}`,
			Scope:       "schedule",
			ChatOnly:    true,
			Run:         e.toolCreateWebhook,
		},
		{
			Name:        "list_jobs",
			Description: "List the user's scheduled and webhook jobs with their last run status.",
			Params:      `{"type":"object","properties":{}}`,
			Scope:       "schedule",
			Run:         e.toolListJobs,
		},
		{
			Name:        "job_history",
			Description: "Show recent runs of a job: ok, nothing found, error or missed.",
			Params:      `{"type":"object","properties":{"job_id":{"type":"integer"}},"required":["job_id"]}`,
			Scope:       "schedule",
			Run:         e.toolJobHistory,
		},
		{
			Name:        "update_job",
			Description: "Pause, resume or delete a scheduled or webhook job. new_url gives a webhook job a fresh URL (and signing secret), DMed to the user, and stops the old ones, e.g. if they leaked. set_signature makes a webhook job require calls signed the way GitHub, Gitea/Forgejo or GitLab sign them (or none).",
			Params:      `{"type":"object","properties":{"job_id":{"type":"integer"},"action":{"type":"string","enum":["pause","resume","delete","new_url","set_signature"]},"signature":{"type":"string","enum":["none","github","gitea","gitlab"],"description":"for set_signature"}},"required":["job_id","action"]}`,
			Scope:       "schedule",
			ChatOnly:    true,
			Run:         e.toolUpdateJob,
		},
		// --- the web (granted per sprig) ---
		{
			Name:        "web_search",
			Description: "Search the web for current information (news, prices, opening hours, facts you're unsure of). Returns results or a sourced answer; then fetch_page a source to read it in full. Say where things came from when you tell the user.",
			Params:      `{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`,
			Scope:       character.ScopeWeb,
			Untrusted:   true,
			Run:         e.toolWebSearch,
		},
		{
			Name:        "fetch_page",
			Description: "Read a web page (an http(s) address the user gave, or one from web_search) as plain text. Only public internet addresses.",
			Params:      `{"type":"object","properties":{"url":{"type":"string"}},"required":["url"]}`,
			Scope:       character.ScopeWeb,
			Untrusted:   true,
			Run:         e.toolFetchPage,
		},
		// --- skills (a shared library of procedures) ---
		{
			Name:        "read_skill",
			Description: "Read a saved skill (a procedure listed under Skills in your instructions) before doing the task it covers. Returns its steps and version.",
			Params:      `{"type":"object","properties":{"id":{"type":"string"}},"required":["id"]}`,
			Run:         e.toolReadSkill,
		},
		{
			Name:        "save_skill",
			Description: "Save a procedure as a skill every sprig can follow, once the user agrees. base_version 0 creates it; to revise, pass the version read_skill returned. Steps are numbered instructions in plain words, no secrets.",
			Params:      `{"type":"object","properties":{"id":{"type":"string","description":"short lowercase id, e.g. morning-briefing"},"name":{"type":"string"},"description":{"type":"string","description":"one sentence: when to use it"},"steps":{"type":"string"},"note":{"type":"string","description":"what changed and why (for the history)"},"base_version":{"type":"integer"}},"required":["id","name","description","steps","base_version"]}`,
			AdminOnly:   true,
			ChatOnly:    true,
			Run:         e.toolSaveSkill,
		},
		{
			Name:        "manage_skill",
			Description: "See a skill's version history, restore an earlier version (as a new version; nothing is lost), or delete a skill.",
			Params:      `{"type":"object","properties":{"id":{"type":"string"},"action":{"type":"string","enum":["history","restore","delete"]},"version":{"type":"integer","description":"for restore"}},"required":["id","action"]}`,
			AdminOnly:   true,
			ChatOnly:    true,
			Run:         e.toolManageSkill,
		},
		// --- characters ---
		{
			Name:        "preview_character",
			Description: "Draft a new sprig and have it introduce itself in chat in its own voice, so the user can see it before it's created. Call again with changes to re-preview.",
			Params:      draftSchema,
			Scope:       "characters",
			ChatOnly:    true,
			Run:         e.toolPreviewCharacter,
		},
		{
			Name:        "create_character",
			Description: "Create the sprig from the most recent preview, once the user is happy with it.",
			Params:      `{"type":"object","properties":{"id":{"type":"string","description":"id of the previewed sprig"}},"required":["id"]}`,
			Scope:       "characters",
			ChatOnly:    true,
			Run:         e.toolCreateCharacter,
		},
		{
			Name:        "grant_tools",
			Description: "Change which tool scopes a sprig has: schedule, web (search the web and read pages), or mcp:<integration>/<tool or *>. Edits its character file (never overwrites manual edits).",
			Params:      `{"type":"object","properties":{"character":{"type":"string"},"add":{"type":"array","items":{"type":"string"}},"remove":{"type":"array","items":{"type":"string"}}},"required":["character"]}`,
			Scope:       "characters",
			AdminOnly:   true,
			ChatOnly:    true,
			Run:         e.toolGrantTools,
		},
		// --- admin ---
		{
			Name:        "settings",
			Description: "Show or change Thicket settings: model, fallback_model, cheap_model, budget.daily_usd, budget.monthly_usd, budget.character.<id>.daily_usd, consistency_check, checkin_days, pdf_engine (auto | native | cloudflare-ai | mistral-ocr), web_search (auto | openrouter | searxng | off), search_url (SearXNG address), history_tokens (how much conversation each reply includes; 0 = automatic), model_url (OpenRouter's API, or a local OpenAI-compatible server; when switching, also set model, cheap_model and fallback_model to model names that server has). Omit name to list them. Empty value resets to default.",
			Params:      `{"type":"object","properties":{"name":{"type":"string"},"value":{"type":"string"}}}`,
			Scope:       "admin",
			AdminOnly:   true,
			Run:         e.toolSettings,
		},
		{
			Name:        "spend_report",
			Description: "Show model spend today and this month, against the caps, broken down by sprig.",
			Params:      `{"type":"object","properties":{}}`,
			Scope:       "admin",
			AdminOnly:   true,
			Run:         e.toolSpend,
		},
		{
			Name:        "recent_activity",
			Description: "Show the recent activity log (tool calls, jobs, errors). level: info, warn or error.",
			Params:      `{"type":"object","properties":{"level":{"type":"string","enum":["info","warn","error"]},"limit":{"type":"integer"}}}`,
			Scope:       "admin",
			AdminOnly:   true,
			Run:         e.toolActivity,
		},
		{
			Name:        "request_secret",
			Description: "Ask the user to send a secret (API key, token) in their next DM. The value is captured by Thicket, stored encrypted and never shown to you. Refer to it in integrations as {{secret:NAME}}.",
			Params:      `{"type":"object","properties":{"name":{"type":"string","description":"lowercase_with_underscores"},"purpose":{"type":"string"}},"required":["name","purpose"]}`,
			Scope:       "admin",
			AdminOnly:   true,
			ChatOnly:    true,
			Run:         e.toolRequestSecret,
		},
		{
			Name:        "add_integration",
			Description: "Connect an MCP server. transport 'http' (target = URL, e.g. Home Assistant's http://homeassistant.local:8123/api/mcp with header Authorization: 'Bearer {{secret:ha_token}}') or 'stdio' (target = command). Use request_secret first for any token. After connecting, grant sprigs access with grant_tools.",
			Params:      `{"type":"object","properties":{"name":{"type":"string","description":"short lowercase name"},"transport":{"type":"string","enum":["http","stdio"]},"target":{"type":"string"},"args":{"type":"array","items":{"type":"string"}},"headers":{"type":"object","additionalProperties":{"type":"string"}},"env":{"type":"object","additionalProperties":{"type":"string"}}},"required":["name","transport","target"]}`,
			Scope:       "admin",
			AdminOnly:   true,
			ChatOnly:    true,
			Run:         e.toolAddIntegration,
		},
		{
			Name:        "list_integrations",
			Description: "List connected MCP integrations, their status and tools.",
			Params:      `{"type":"object","properties":{}}`,
			Scope:       "admin",
			AdminOnly:   true,
			Run:         e.toolListIntegrations,
		},
		{
			Name:        "remove_integration",
			Description: "Disconnect and remove an MCP integration.",
			Params:      `{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`,
			Scope:       "admin",
			AdminOnly:   true,
			ChatOnly:    true,
			Run:         e.toolRemoveIntegration,
		},
		{
			Name:        "set_tool_level",
			Description: "Set a tool's permission level, for one sprig or (sprig omitted) all of them: read (may only read), draft (changes are written up for the user, never done), ask (changes need the user's approval), act (no approval), or default (integration tools that change things ask; others act).",
			Params:      `{"type":"object","properties":{"tool":{"type":"string"},"level":{"type":"string","enum":["read","draft","ask","act","default"]},"sprig":{"type":"string","description":"sprig id or name; omit for every sprig"}},"required":["tool","level"]}`,
			Scope:       "admin",
			AdminOnly:   true,
			ChatOnly:    true,
			Run:         e.toolSetLevel,
		},
		{
			Name:        "invite_user",
			Description: "Let another person talk to this Thicket. Pass their numeric user id on Discord (or an @mention) or Telegram, and which app. Their memories are kept separate. To use Thicket from a second app themselves, people use link_account instead.",
			Params:      `{"type":"object","properties":{"user":{"type":"string"},"app":{"type":"string","enum":["discord","telegram"],"description":"default: the app you're talking on"},"admin":{"type":"boolean"}},"required":["user"]}`,
			Scope:       "admin",
			AdminOnly:   true,
			ChatOnly:    true,
			Run:         e.toolInvite,
		},
		{
			Name:        "link_account",
			Description: "When the user wants to talk to Thicket from another chat app too (Discord or Telegram) with the same sprigs and memories: Thicket DMs them a one-time code to send from the other app. You never see the code.",
			Params:      `{"type":"object","properties":{}}`,
			Scope:       "host",
			ChatOnly:    true,
			Run:         e.toolLinkAccount,
		},
		{
			Name:        "web_app_link",
			Description: "Post a one-time sign-in link to the Thicket web app, where the user can chat with every sprig side by side; once signed in, its read-only dashboard (/dashboard: memories, jobs, skills, and spending for admins) works too. Thicket posts the link itself right after your reply; never write or guess a URL.",
			Params:      `{"type":"object","properties":{}}`,
			Scope:       "host",
			ChatOnly:    true,
			Run:         e.toolWebLink,
		},
		{
			Name:        "memory_reviews",
			Description: "List recent automatic memory review runs, or roll one back (undoing every change it made) with rollback_run_id.",
			Params:      `{"type":"object","properties":{"rollback_run_id":{"type":"integer"}}}`,
			Scope:       "admin",
			AdminOnly:   true,
			Run:         e.toolMemoryReviews,
		},
	}
}

// --- memory tools ---

func (e *Engine) toolRemember(ctx context.Context, t *turn, args json.RawMessage) (string, error) {
	a, err := decode[struct {
		Content   string `json:"content"`
		Scope     string `json:"scope"`
		Kind      string `json:"kind"`
		UserAsked bool   `json:"user_asked"`
	}](args)
	if err != nil {
		return "", err
	}
	content := strings.Join(strings.Fields(a.Content), " ")
	if n := utf8.RuneCountInString(content); n < 3 || n > memory.MaxContent {
		return "", fmt.Errorf("content must be 3-%d characters", memory.MaxContent)
	}
	if secrets.LooksLikeSecret(content) {
		return "", errors.New("that looks like a credential; secrets are never stored as memories")
	}
	kind := a.Kind
	if kind == "" {
		kind = "fact"
	}
	if !memory.Kinds[kind] || kind == "followup" {
		return "", fmt.Errorf("invalid kind %q", kind)
	}
	char := ""
	if a.Scope == "mine" {
		char = t.char.ID
	}
	existing, err := e.DB.VisibleMemories(ctx, t.user.ID, t.char.ID)
	if err != nil {
		return "", err
	}
	for _, m := range existing {
		if m.Character == char && memory.Similarity(m.Content, content) >= 0.75 {
			if m.Locked && !a.UserAsked {
				return fmt.Sprintf("Already remembered as #%d (set by the user).", m.ID), nil
			}
			ch := store.MemoryChange{Action: "update", Memory: store.Memory{ID: m.ID, Content: content, Kind: kind}}
			if _, err := e.DB.ApplyMemoryChanges(ctx, 0, []store.MemoryChange{ch}); err != nil {
				return "", err
			}
			if a.UserAsked && !m.Locked {
				_ = e.DB.LockMemory(ctx, m.ID)
			}
			return fmt.Sprintf("Updated memory #%d.", m.ID), nil
		}
	}
	m := store.Memory{UserID: t.user.ID, Character: char, Kind: kind, Content: content, Source: "auto", Locked: a.UserAsked}
	if a.UserAsked {
		m.Source = "user"
	}
	if kind == "context" {
		m.ExpiresAt = e.now().Add(memory.ContextTTL)
	}
	ids, err := e.DB.ApplyMemoryChanges(ctx, 0, []store.MemoryChange{{Action: "add", Memory: m}})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Remembered (#%d).", ids[0]), nil
}

func (e *Engine) toolListMemories(ctx context.Context, t *turn, _ json.RawMessage) (string, error) {
	mems, err := e.DB.VisibleMemories(ctx, t.user.ID, t.char.ID)
	if err != nil {
		return "", err
	}
	if len(mems) == 0 {
		return "No memories yet.", nil
	}
	loc := t.user.Location()
	var b strings.Builder
	for _, m := range mems {
		scope := "shared"
		if m.Character != "" {
			scope = "just us"
		}
		extra := ""
		if m.Locked {
			extra = ", you told me"
		}
		if !m.DueAt.IsZero() {
			extra += ", follow up " + m.DueAt.In(loc).Format("Mon 2 Jan 15:04")
		}
		fmt.Fprintf(&b, "#%d [%s, %s%s] %s\n", m.ID, scope, m.Kind, extra, m.Content)
	}
	return b.String(), nil
}

func (e *Engine) toolForget(ctx context.Context, t *turn, args json.RawMessage) (string, error) {
	a, err := decode[struct {
		IDs []int64 `json:"ids"`
	}](args)
	if err != nil {
		return "", err
	}
	visible, err := e.DB.VisibleMemories(ctx, t.user.ID, t.char.ID)
	if err != nil {
		return "", err
	}
	ok := map[int64]bool{}
	for _, m := range visible {
		ok[m.ID] = true
	}
	var changes []store.MemoryChange
	var missing []string
	for _, id := range a.IDs {
		if !ok[id] {
			missing = append(missing, fmt.Sprint(id))
			continue
		}
		changes = append(changes, store.MemoryChange{Action: "delete", Memory: store.Memory{ID: id}})
	}
	if _, err := e.DB.ApplyMemoryChanges(ctx, 0, changes); err != nil {
		return "", err
	}
	out := fmt.Sprintf("Forgot %d memor(y/ies) for good.", len(changes))
	if len(missing) > 0 {
		out += " Not found or not yours: " + strings.Join(missing, ", ")
	}
	return out, nil
}

func (e *Engine) toolFollowup(ctx context.Context, t *turn, args json.RawMessage) (string, error) {
	a, err := decode[struct {
		About string `json:"about"`
		Due   string `json:"due"`
	}](args)
	if err != nil {
		return "", err
	}
	plan := memory.BuildPlan(memory.PlanInput{UserID: t.user.ID, Scopes: map[string]bool{t.char.ID: true}, Now: e.now(), Loc: t.user.Location()},
		[]memory.Op{{Op: "add", Scope: t.char.ID, Kind: "followup", Content: a.About, Due: a.Due}})
	if len(plan.Changes) == 0 {
		return "", errors.New(strings.Join(plan.Skipped, "; "))
	}
	if _, err := e.DB.ApplyMemoryChanges(ctx, 0, plan.Changes); err != nil {
		return "", err
	}
	return "Follow-up noted.", nil
}

func (e *Engine) toolHandoff(ctx context.Context, t *turn, args json.RawMessage) (string, error) {
	a, err := decode[struct {
		To      string `json:"to"`
		Message string `json:"message"`
	}](args)
	if err != nil {
		return "", err
	}
	target, ok := e.Chars.Get(a.To)
	if !ok {
		if target, ok = e.Chars.FindByName(a.To); !ok {
			return "", fmt.Errorf("there's no sprig called %q", a.To)
		}
	}
	if target.ID == t.char.ID {
		return "", errors.New("you can't hand off to yourself")
	}
	if t.depth >= maxHandoffDepth {
		return "", errors.New("too many handoffs in a row; answer the user yourself")
	}
	t.handoff = &handoffReq{to: target.ID, message: a.Message}
	return fmt.Sprintf("%s will pick this up right after your message. Keep your reply to a one-line hand-over.", target.DisplayName()), nil
}

// --- onboarding ---

func (e *Engine) toolSetProfile(ctx context.Context, t *turn, args json.RawMessage) (string, error) {
	a, err := decode[struct {
		Name     string `json:"name"`
		Timezone string `json:"timezone"`
	}](args)
	if err != nil {
		return "", err
	}
	a.Name = strings.TrimSpace(a.Name)
	if len(a.Name) > 60 {
		return "", errors.New("name is too long")
	}
	if a.Timezone != "" {
		if _, err := time.LoadLocation(a.Timezone); err != nil {
			return "", fmt.Errorf("unknown timezone %q; use an IANA name like Europe/London", a.Timezone)
		}
	}
	if err := e.DB.UpdateUserProfile(ctx, t.user.ID, a.Name, a.Timezone); err != nil {
		return "", err
	}
	if a.Name != "" {
		t.user.Name = a.Name
	}
	if a.Timezone != "" {
		t.user.Timezone = a.Timezone
	}
	return "Profile saved.", nil
}

// --- schedule ---

func (e *Engine) toolScheduleJob(ctx context.Context, t *turn, args json.RawMessage) (string, error) {
	a, err := decode[struct {
		Name     string `json:"name"`
		Schedule string `json:"schedule"`
		Task     string `json:"task"`
		Owner    string `json:"owner"`
	}](args)
	if err != nil {
		return "", err
	}
	owner := t.char
	if a.Owner != "" && a.Owner != t.char.ID {
		c, ok := e.Chars.Get(a.Owner)
		if !ok {
			return "", fmt.Errorf("there's no sprig with id %q", a.Owner)
		}
		owner = c
	}
	if strings.TrimSpace(a.Name) == "" || strings.TrimSpace(a.Task) == "" || len(a.Task) > 2000 {
		return "", errors.New("name and task are required (task max 2000 characters)")
	}
	loc := t.user.Location()
	next, err := scheduler.Describe(a.Schedule, loc, e.now(), 3)
	if err != nil {
		return "", err
	}
	j, err := e.DB.CreateJob(ctx, store.Job{UserID: t.user.ID, Character: owner.ID, Name: a.Name, Schedule: a.Schedule, Prompt: a.Task})
	if err != nil {
		return "", err
	}
	e.logActivity(ctx, "info", "job", owner.ID, t.user.ID, fmt.Sprintf("job #%d %q scheduled (%s)", j.ID, j.Name, j.Schedule), j.Prompt)
	var ts []string
	for _, n := range next {
		ts = append(ts, n.Format("Mon 2 Jan 15:04"))
	}
	note := ""
	if t.user.Timezone == "" {
		note = " The user's timezone isn't set, so times are UTC; ask where they are and call set_user_profile."
	}
	return fmt.Sprintf("Job #%d created, run by %s. Next runs (%s): %s.%s", j.ID, owner.DisplayName(), loc, strings.Join(ts, "; "), note), nil
}

func (e *Engine) toolListJobs(ctx context.Context, t *turn, _ json.RawMessage) (string, error) {
	jobs, err := e.DB.UserJobs(ctx, t.user.ID)
	if err != nil {
		return "", err
	}
	if len(jobs) == 0 {
		return "No scheduled jobs.", nil
	}
	loc := t.user.Location()
	var b strings.Builder
	for _, j := range jobs {
		state := "active"
		if !j.Enabled {
			state = "paused"
		}
		last := "never run"
		if runs, _ := e.DB.JobRuns(ctx, j.ID, 1); len(runs) > 0 {
			last = fmt.Sprintf("last run %s: %s", runs[0].StartedAt.In(loc).Format("Mon 2 Jan 15:04"), runs[0].Status)
		}
		if j.Kind == store.JobWebhook {
			fmt.Fprintf(&b, "#%d %q by %s, webhook (%s), %s\n", j.ID, j.Name, e.charName(j.Character), state, last)
			continue
		}
		next := ""
		if j.Enabled {
			if n := scheduler.Next(j.Schedule, e.now(), loc); !n.IsZero() {
				next = ", next " + n.Format("Mon 2 Jan 15:04")
			}
		}
		fmt.Fprintf(&b, "#%d %q by %s, %s (%s), %s%s\n", j.ID, j.Name, e.charName(j.Character), j.Schedule, state, last, next)
	}
	return b.String(), nil
}

func (e *Engine) ownedJob(ctx context.Context, t *turn, id int64) (*store.Job, error) {
	j, err := e.DB.JobByID(ctx, id)
	if err != nil || (j.UserID != t.user.ID && !t.user.IsAdmin()) {
		return nil, fmt.Errorf("no job #%d", id)
	}
	return j, nil
}

func (e *Engine) toolJobHistory(ctx context.Context, t *turn, args json.RawMessage) (string, error) {
	a, err := decode[struct {
		JobID int64 `json:"job_id"`
	}](args)
	if err != nil {
		return "", err
	}
	j, err := e.ownedJob(ctx, t, a.JobID)
	if err != nil {
		return "", err
	}
	runs, err := e.DB.JobRuns(ctx, j.ID, 10)
	if err != nil {
		return "", err
	}
	if len(runs) == 0 {
		return "No runs yet.", nil
	}
	loc := t.user.Location()
	var b strings.Builder
	for _, r := range runs {
		fmt.Fprintf(&b, "%s: %s", r.StartedAt.In(loc).Format("Mon 2 Jan 15:04"), r.Status)
		if r.Error != "" {
			fmt.Fprintf(&b, " (%s)", truncate(r.Error, 200))
		}
		if r.CostUSD > 0 {
			fmt.Fprintf(&b, " $%.4f", r.CostUSD)
		}
		b.WriteString("\n")
	}
	return b.String(), nil
}

func (e *Engine) toolUpdateJob(ctx context.Context, t *turn, args json.RawMessage) (string, error) {
	a, err := decode[struct {
		JobID     int64  `json:"job_id"`
		Action    string `json:"action"`
		Signature string `json:"signature"`
	}](args)
	if err != nil {
		return "", err
	}
	j, err := e.ownedJob(ctx, t, a.JobID)
	if err != nil {
		return "", err
	}
	switch a.Action {
	case "new_url":
		return e.regenerateHook(ctx, t, j)
	case "set_signature":
		return e.setHookSignature(ctx, t, j, a.Signature)
	case "pause":
		err = e.DB.SetJobEnabled(ctx, j.ID, false)
	case "resume":
		err = e.DB.SetJobEnabled(ctx, j.ID, true)
	case "delete":
		err = e.DB.DeleteJob(ctx, j.ID)
		if err == nil && j.HookAuth != "" {
			_ = e.Secrets.Delete(ctx, hookSecretName(j.ID))
		}
	default:
		return "", fmt.Errorf("unknown action %q", a.Action)
	}
	if err != nil {
		return "", err
	}
	e.logActivity(ctx, "info", "job", j.Character, t.user.ID, fmt.Sprintf("job #%d %s", j.ID, a.Action+"d"), "")
	return fmt.Sprintf("Job #%d %sd.", j.ID, a.Action), nil
}

// --- characters ---

const draftSchema = `{"type":"object","properties":{
"id":{"type":"string","description":"short lowercase id, e.g. pip"},
"name":{"type":"string"},
"nickname":{"type":"string"},
"role":{"type":"string","description":"one sentence: who they are and what they're for"},
"traits":{"type":"array","items":{"type":"string"},"description":"3-4 core traits"},
"quirks":{"type":"array","items":{"type":"string"}},
"catchphrases":{"type":"array","items":{"type":"string"}},
"voice_style":{"type":"array","items":{"type":"string"},"description":"rules for how they talk"},
"never_say":{"type":"array","items":{"type":"string"}},
"relationship_to_user":{"type":"string"},
"relationships":{"type":"object","additionalProperties":{"type":"string"},"description":"how they see other sprigs, keyed by id"},
"tools":{"type":"array","items":{"type":"string"},"description":"scopes they need: schedule, web, mcp:<integration>/<tool or *>"},
"avatar":{"type":"string","description":"https URL of an avatar image, if the user has one"},
"color":{"type":"string","description":"hex like #e0a458"},
"background":{"type":"string","description":"a short paragraph of backstory and a few example lines"},
"check_ins":{"type":"boolean"}
},"required":["id","name","role","traits","voice_style"]}`

type draftArgs struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	Nickname      string            `json:"nickname"`
	Role          string            `json:"role"`
	Traits        []string          `json:"traits"`
	Quirks        []string          `json:"quirks"`
	Catchphrases  []string          `json:"catchphrases"`
	VoiceStyle    []string          `json:"voice_style"`
	NeverSay      []string          `json:"never_say"`
	RelToUser     string            `json:"relationship_to_user"`
	Relationships map[string]string `json:"relationships"`
	Tools         []string          `json:"tools"`
	Avatar        string            `json:"avatar"`
	Color         string            `json:"color"`
	Background    string            `json:"background"`
	CheckIns      bool              `json:"check_ins"`
}

func (e *Engine) toolPreviewCharacter(ctx context.Context, t *turn, args json.RawMessage) (string, error) {
	a, err := decode[draftArgs](args)
	if err != nil {
		return "", err
	}
	c := &character.Character{
		ID: strings.ToLower(strings.TrimSpace(a.ID)), Name: strings.TrimSpace(a.Name), Nickname: strings.TrimSpace(a.Nickname),
		Role: a.Role, Avatar: a.Avatar, Color: a.Color,
		Personality:   character.Personality{Traits: a.Traits, Quirks: a.Quirks, Catchphrases: a.Catchphrases},
		Voice:         character.Voice{Style: a.VoiceStyle, NeverSay: a.NeverSay},
		Relationships: character.Relationships{User: a.RelToUser, Characters: a.Relationships},
		Tools:         a.Tools,
		Initiative:    character.Initiative{CheckIns: a.CheckIns, FollowUps: true, QuietHours: "22:00-08:00"},
		CreatedBy:     t.char.ID,
		Body:          strings.TrimSpace(a.Background),
	}
	if c.Avatar != "" && !strings.HasPrefix(c.Avatar, "https://") {
		c.Avatar = ""
	}
	if c.Color == "" {
		c.Color = "#7a9e7e"
	}
	probs := c.Validate()
	for _, s := range c.Tools {
		if s == character.ScopeAdmin || s == character.ScopeCharacters {
			probs = append(probs, "new sprigs can't be given the "+s+" scope from chat")
		}
		if strings.HasPrefix(s, character.ScopeMCPPrefix) && !t.user.IsAdmin() {
			probs = append(probs, "only the owner or an admin can give a sprig integration access ("+s+")")
		}
	}
	if _, exists := e.Chars.Get(c.ID); exists {
		probs = append(probs, fmt.Sprintf("a sprig with id %q already exists; pick another id", c.ID))
	}
	if len(probs) > 0 {
		return "", errors.New("the draft needs fixing: " + strings.Join(probs, "; "))
	}
	// The draft introduces itself, in its own voice.
	sys := buildSystemPrompt(promptInput{char: c, user: t.user, roster: append(e.Chars.All(), c), now: e.now(), onboarded: true, isDM: t.isDM})
	resp, err := e.LLM.Chat(llm.WithMeta(ctx, llm.Meta{UserID: t.user.ID, Character: t.char.ID, Purpose: "preview"}), llm.Request{
		Messages: []llm.Message{
			{Role: "system", Content: sys},
			{Role: "user", Content: fmt.Sprintf("[Thicket event] %s is introducing you to the user for the first time. Say hello and introduce yourself in two or three sentences, fully in your own voice.", t.char.DisplayName())},
		},
		MaxTokens: 400,
	})
	if err != nil {
		return "", fmt.Errorf("couldn't generate the preview: %w", err)
	}
	intro := cleanReply(resp.Message.Content, c)
	preview := *c
	preview.Nickname = c.DisplayName() + " (preview)"
	if err := e.send(ctx, t.chatRef, &preview, intro); err != nil {
		return "", err
	}
	if t.conv != nil {
		_ = e.DB.AddMessage(ctx, t.conv.ID, "event", "", fmt.Sprintf("Preview of new sprig %s introduced itself: %s", c.DisplayName(), intro))
	}
	e.drafts.Store(t.user.ID, c)
	return fmt.Sprintf("The preview of %s has been posted in the chat (it said: %q). Ask the user what they think and whether anything feels off. Don't repeat the intro.", c.DisplayName(), intro), nil
}

func (e *Engine) toolCreateCharacter(ctx context.Context, t *turn, args json.RawMessage) (string, error) {
	a, err := decode[struct {
		ID string `json:"id"`
	}](args)
	if err != nil {
		return "", err
	}
	v, ok := e.drafts.Load(t.user.ID)
	if !ok {
		return "", errors.New("there's no preview to create; call preview_character first so the user can see the sprig")
	}
	c := v.(*character.Character)
	if c.ID != strings.ToLower(a.ID) {
		return "", fmt.Errorf("the latest preview is %q, not %q", c.ID, a.ID)
	}
	path, err := e.Chars.Write(ctx, c)
	if errors.Is(err, character.ErrUserEdited) {
		return "", fmt.Errorf("a file for %q already exists and was edited by hand; I saved my version to %s instead of overwriting it", c.ID, path)
	}
	if err != nil {
		return "", err
	}
	e.drafts.Delete(t.user.ID)
	e.logActivity(ctx, "info", "character", c.ID, t.user.ID, fmt.Sprintf("new sprig %s sprouted", c.DisplayName()), path)
	return fmt.Sprintf("%s has been created (%s). They can now be addressed by name, e.g. \"%s, ...\".", c.DisplayName(), path, c.DisplayName()), nil
}

func (e *Engine) toolGrantTools(ctx context.Context, t *turn, args json.RawMessage) (string, error) {
	a, err := decode[struct {
		Character string   `json:"character"`
		Add       []string `json:"add"`
		Remove    []string `json:"remove"`
	}](args)
	if err != nil {
		return "", err
	}
	c, ok := e.Chars.Get(a.Character)
	if !ok {
		return "", fmt.Errorf("no sprig with id %q", a.Character)
	}
	cp := *c
	set := map[string]bool{}
	for _, s := range c.Tools {
		set[s] = true
	}
	for _, s := range a.Add {
		if (s == character.ScopeAdmin || s == character.ScopeCharacters) && !c.Host {
			return "", fmt.Errorf("the %s scope can only be granted by editing the character file", s)
		}
		set[s] = true
	}
	for _, s := range a.Remove {
		delete(set, s)
	}
	cp.Tools = nil
	for s := range set {
		cp.Tools = append(cp.Tools, s)
	}
	sort.Strings(cp.Tools)
	path, err := e.Chars.Write(ctx, &cp)
	if errors.Is(err, character.ErrUserEdited) {
		return fmt.Sprintf("%s's file was edited by hand, so I didn't overwrite it. My proposed version is at %s; the user can merge it.", c.DisplayName(), path), nil
	}
	if err != nil {
		return "", err
	}
	e.logActivity(ctx, "info", "character", c.ID, t.user.ID, "tool scopes changed", strings.Join(cp.Tools, ", "))
	return fmt.Sprintf("%s now has: %s.", c.DisplayName(), strings.Join(cp.Tools, ", ")), nil
}

// --- admin ---

func (e *Engine) toolSettings(ctx context.Context, t *turn, args json.RawMessage) (string, error) {
	a, err := decode[struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}](args)
	if err != nil {
		return "", err
	}
	if a.Name == "" {
		return strings.Join(e.Settings.Describe(ctx), "\n"), nil
	}
	if err := e.Settings.Set(ctx, a.Name, a.Value); err != nil {
		return "", err
	}
	e.logActivity(ctx, "info", "system", t.char.ID, t.user.ID, "setting changed: "+a.Name, a.Value)
	return fmt.Sprintf("%s is now %s.", a.Name, e.Settings.String(ctx, a.Name)), nil
}

func (e *Engine) toolSpend(ctx context.Context, t *turn, _ json.RawMessage) (string, error) {
	status, err := e.Budget.Status(ctx)
	if err != nil {
		return "", err
	}
	now := e.now().UTC()
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	rows, err := e.DB.SpendBreakdown(ctx, month, "character")
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(status)
	b.WriteString("This month by sprig:\n")
	for _, r := range rows {
		name := r.Key
		if name == "" {
			name = "(background: memory review)"
		}
		fmt.Fprintf(&b, "- %s: $%.4f (%d tokens)\n", name, r.CostUSD, r.Tokens)
	}
	return b.String(), nil
}

func (e *Engine) toolActivity(ctx context.Context, t *turn, args json.RawMessage) (string, error) {
	a, _ := decode[struct {
		Level string `json:"level"`
		Limit int    `json:"limit"`
	}](args)
	if a.Limit <= 0 || a.Limit > 50 {
		a.Limit = 20
	}
	acts, err := e.DB.RecentActivity(ctx, a.Limit, a.Level)
	if err != nil {
		return "", err
	}
	loc := t.user.Location()
	var b strings.Builder
	for _, x := range acts {
		fmt.Fprintf(&b, "%s [%s/%s] %s %s\n", x.At.In(loc).Format("Jan 2 15:04"), x.Level, x.Kind, x.Character, x.Summary)
	}
	if b.Len() == 0 {
		return "Nothing logged.", nil
	}
	return e.Secrets.Redact(b.String()), nil
}

func (e *Engine) toolRequestSecret(ctx context.Context, t *turn, args json.RawMessage) (string, error) {
	a, err := decode[struct {
		Name    string `json:"name"`
		Purpose string `json:"purpose"`
	}](args)
	if err != nil {
		return "", err
	}
	if !secrets.ValidName(a.Name) {
		return "", errors.New("secret names are lowercase letters, digits and underscores")
	}
	if !t.isDM {
		return "", errors.New("secrets can only be collected in a DM; ask the user to DM you")
	}
	if err := e.DB.SetPendingSecret(ctx, store.PendingSecret{UserID: t.user.ID, Name: a.Name, Purpose: a.Purpose}); err != nil {
		return "", err
	}
	return fmt.Sprintf("Ready. Ask the user to send the %s as their next message here (or 'cancel'). Thicket will capture and encrypt it; you will never see the value. Refer to it as {{secret:%s}}.", a.Purpose, a.Name), nil
}

func (e *Engine) toolAddIntegration(ctx context.Context, t *turn, args json.RawMessage) (string, error) {
	a, err := decode[store.MCPServer](args)
	if err != nil {
		return "", err
	}
	if !secrets.ValidName(a.Name) || strings.Contains(a.Name, "__") {
		return "", errors.New("integration names are lowercase letters, digits and single underscores")
	}
	for k, v := range a.Headers {
		if secrets.LooksLikeSecret(v) {
			return "", fmt.Errorf("header %s contains a raw credential; use request_secret and {{secret:NAME}} instead", k)
		}
	}
	for k, v := range a.Env {
		if secrets.LooksLikeSecret(v) {
			return "", fmt.Errorf("env %s contains a raw credential; use request_secret and {{secret:NAME}} instead", k)
		}
	}
	a.Enabled = true
	if err := e.DB.PutMCPServer(ctx, a); err != nil {
		return "", err
	}
	tools, err := e.MCP.Connect(ctx, a)
	if err != nil {
		e.logActivity(ctx, "warn", "tool", t.char.ID, t.user.ID, "integration "+a.Name+" failed to connect", err.Error())
		return "", fmt.Errorf("saved, but connecting failed: %v", err)
	}
	var names []string
	for _, tl := range tools {
		ro := ""
		if !tl.ReadOnly() {
			ro = " (needs approval)"
		}
		names = append(names, tl.Name+ro)
	}
	e.logActivity(ctx, "info", "tool", t.char.ID, t.user.ID, "integration "+a.Name+" connected", strings.Join(names, ", "))
	return fmt.Sprintf("Connected %s with %d tools: %s. No sprig can use them yet: ask the user who should, then grant_tools with mcp:%s/* or specific tools.", a.Name, len(tools), strings.Join(names, ", "), a.Name), nil
}

func (e *Engine) toolListIntegrations(ctx context.Context, t *turn, _ json.RawMessage) (string, error) {
	servers, err := e.DB.MCPServers(ctx)
	if err != nil {
		return "", err
	}
	if len(servers) == 0 {
		return "No integrations yet.", nil
	}
	status := e.MCP.Status()
	byServer := map[string][]string{}
	for _, st := range e.MCP.Tools() {
		byServer[st.Server] = append(byServer[st.Server], st.Tool.Name)
	}
	var b strings.Builder
	for _, s := range servers {
		st := status[s.Name]
		if st == "" {
			st = "not connected"
		}
		var who []string
		for _, c := range e.Chars.All() {
			for _, sc := range c.Tools {
				if strings.HasPrefix(sc, "mcp:"+s.Name) {
					who = append(who, c.DisplayName())
					break
				}
			}
		}
		fmt.Fprintf(&b, "%s (%s %s): %s. Tools: %s. Used by: %s\n", s.Name, s.Transport, s.Target, st,
			strings.Join(byServer[s.Name], ", "), strings.Join(who, ", "))
	}
	return b.String(), nil
}

func (e *Engine) toolRemoveIntegration(ctx context.Context, t *turn, args json.RawMessage) (string, error) {
	a, err := decode[struct {
		Name string `json:"name"`
	}](args)
	if err != nil {
		return "", err
	}
	e.MCP.Disconnect(a.Name)
	if err := e.DB.DeleteMCPServer(ctx, a.Name); err != nil {
		return "", err
	}
	e.logActivity(ctx, "info", "tool", t.char.ID, t.user.ID, "integration "+a.Name+" removed", "")
	return "Removed " + a.Name + ". Character files still list its scope; that's harmless.", nil
}

func (e *Engine) toolInvite(ctx context.Context, t *turn, args json.RawMessage) (string, error) {
	a, err := decode[struct {
		User  string `json:"user"`
		App   string `json:"app"`
		Admin bool   `json:"admin"`
	}](args)
	if err != nil {
		return "", err
	}
	app := strings.ToLower(a.App)
	if app == "" {
		app, _ = splitRef(t.chatRef)
		if app == "" {
			app = t.user.Channel
		}
	}
	if e.channels[app] == nil {
		return "", fmt.Errorf("this Thicket isn't connected to %q", app)
	}
	id := strings.Trim(a.User, "<@!> ")
	if id == "" {
		return "", errors.New("pass their numeric user id")
	}
	for _, r := range id {
		if r < '0' || r > '9' {
			if app == "telegram" {
				return "", errors.New("Telegram bots can't look people up by @username: pass their numeric user id (they can get it from @userinfobot)")
			}
			return "", errors.New("pass a Discord user id or @mention")
		}
	}
	role := "member"
	if a.Admin {
		role = "admin"
	}
	if u, err := e.DB.UserByExternal(ctx, app, id); err == nil {
		if u.Role != "owner" {
			_ = e.DB.SetUserRole(ctx, u.ID, role)
		}
		return "They already have access; role is now " + role + ".", nil
	}
	if _, err := e.DB.CreateUser(ctx, app, id, "", role); err != nil {
		return "", err
	}
	e.logActivity(ctx, "info", "system", t.char.ID, t.user.ID, "invited "+app+" user "+id+" as "+role, "")
	return "Invited. When they DM me I'll welcome them; their memories stay separate from yours.", nil
}

func (e *Engine) toolMemoryReviews(ctx context.Context, t *turn, args json.RawMessage) (string, error) {
	a, _ := decode[struct {
		Rollback int64 `json:"rollback_run_id"`
	}](args)
	if a.Rollback > 0 {
		n, err := e.DB.RollbackRun(ctx, a.Rollback)
		if err != nil {
			return "", err
		}
		e.logActivity(ctx, "info", "memory", t.char.ID, t.user.ID, fmt.Sprintf("memory review run %d rolled back (%d changes)", a.Rollback, n), "")
		return fmt.Sprintf("Rolled back run %d: undid %d change(s).", a.Rollback, n), nil
	}
	runs, err := e.DB.ListExtractionRuns(ctx, 10)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, r := range runs {
		rb := ""
		if !r.RolledBackAt.IsZero() {
			rb = " (rolled back)"
		}
		fmt.Fprintf(&b, "run %d %s: %s, %d applied, %d skipped%s\n", r.ID, r.CreatedAt.Format("Jan 2 15:04"), r.Status, r.Applied, r.Skipped, rb)
	}
	if b.Len() == 0 {
		return "No memory reviews yet.", nil
	}
	return b.String(), nil
}

// webLinkTTL is how long a sign-in link posted in chat stays valid.
const webLinkTTL = 15 * time.Minute

// toolWebLink posts a sign-in link outside the model: the token never
// reaches the provider, the history or the logs.
func (e *Engine) toolWebLink(ctx context.Context, t *turn, _ json.RawMessage) (string, error) {
	if !t.isDM {
		return "Sign-in links are private: tell the user to ask for one in a DM.", nil
	}
	if strings.HasPrefix(t.chatRef, "web:") {
		return "The user is already in the web app.", nil
	}
	token, err := e.DB.CreateWebLink(ctx, t.user.ID, webLinkTTL)
	if err != nil {
		return "", err
	}
	t.afterReply = append(t.afterReply, post{text: "🔑 Your web app sign-in link (works once, for 15 minutes):\n" + e.Cfg.WebBaseURL() + "/login#" + token})
	e.logActivity(ctx, "info", "system", t.char.ID, t.user.ID, "web sign-in link issued", "")
	note := "The link will be posted right after your reply."
	if e.Cfg.PublicURL == "" {
		note += " THICKET_PUBLIC_URL isn't set, so it points at localhost: it only works on the machine running Thicket unless they swap in the right address."
	}
	return note, nil
}
