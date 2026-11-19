package app

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/theshield2594/thicket/internal/api"
	"github.com/theshield2594/thicket/internal/store"
)

type apiResp struct {
	Code    int
	Error   string      `json:"error"`
	Replies []api.Reply `json:"replies"`
	Conv    string      `json:"conversation"`
}

func apiCall(t *testing.T, srv *httptest.Server, token, method, path string, body any) apiResp {
	t.Helper()
	var r *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	} else {
		r = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, srv.URL+path, r)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out apiResp
	json.NewDecoder(resp.Body).Decode(&out)
	out.Code = resp.StatusCode
	return out
}

func setupAPI(t *testing.T) (*App, *fakeChannel, *fakeModel, context.Context, *httptest.Server, string) {
	t.Helper()
	a, fc, fm, ctx := setup(t)
	onboard(t, a, fc, ctx)
	owner, _ := a.DB.UserByExternal(ctx, "discord", "owner")
	token, hash, err := api.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.DB.CreateAPIToken(ctx, owner.ID, "test", hash); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a.API.Handler())
	t.Cleanup(srv.Close)
	return a, fc, fm, ctx, srv, token
}

func TestAPIMessages(t *testing.T) {
	a, fc, _, ctx, srv, token := setupAPI(t)
	if r := apiCall(t, srv, "", "POST", "/api/v1/messages", map[string]string{"text": "hi"}); r.Code != 401 {
		t.Fatalf("no token: %d", r.Code)
	}
	if r := apiCall(t, srv, api.TokenPrefix+"nope", "POST", "/api/v1/messages", map[string]string{"text": "hi"}); r.Code != 401 {
		t.Fatalf("bad token: %d", r.Code)
	}
	if r := apiCall(t, srv, token, "POST", "/api/v1/messages", map[string]string{"text": "  "}); r.Code != 400 {
		t.Fatalf("empty text: %d", r.Code)
	}

	r := apiCall(t, srv, token, "POST", "/api/v1/messages", map[string]string{"text": "hello there"})
	if r.Code != 200 || r.Conv != "default" || len(r.Replies) != 1 || r.Replies[0].SprigID != "juniper" || !strings.Contains(r.Replies[0].Text, "Juni") {
		t.Fatalf("reply: %+v", r)
	}
	// Replies come back over HTTP, not through a chat app, and the user's
	// chat app stays where proactive messages go.
	if out := fc.take(); len(out) != 0 {
		t.Fatalf("API reply leaked to Discord: %+v", out)
	}
	owner, _ := a.DB.UserByExternal(ctx, "discord", "owner")
	if owner.Channel != "discord" {
		t.Fatalf("API became the user's primary app: %+v", owner)
	}

	// Addressing a sprig, and a handoff returning both voices.
	os.WriteFile(filepath.Join(a.Cfg.CharactersDir, "pip.md"), []byte(pip), 0o644)
	if err := a.Chars.Load(); err != nil {
		t.Fatal(err)
	}
	r = apiCall(t, srv, token, "POST", "/api/v1/messages", map[string]string{"text": "what's on?", "sprig": "Pip", "conversation": "pip-chat"})
	if r.Code != 200 || len(r.Replies) != 1 || r.Replies[0].SprigID != "pip" || r.Conv != "pip-chat" {
		t.Fatalf("addressed reply: %+v", r)
	}
	if r := apiCall(t, srv, token, "POST", "/api/v1/messages", map[string]string{"text": "hi", "sprig": "nobody"}); r.Code != 404 {
		t.Fatalf("unknown sprig: %d", r.Code)
	}
	r = apiCall(t, srv, token, "POST", "/api/v1/messages", map[string]string{"text": "can someone help with my calendar?", "sprig": "juniper"})
	if len(r.Replies) != 2 || r.Replies[0].SprigID != "juniper" || r.Replies[1].SprigID != "pip" {
		t.Fatalf("handoff over the API: %+v", r)
	}

	// Approvals work across requests in one conversation.
	a.DB.SetToolLevel(ctx, store.AllSprigs, "remember", store.LevelAsk)
	r = apiCall(t, srv, token, "POST", "/api/v1/messages", map[string]string{"text": "remember my dog is Biscuit", "sprig": "juniper", "conversation": "dog"})
	if len(r.Replies) != 2 || !strings.Contains(r.Replies[1].Text, "I need your OK") {
		t.Fatalf("approval prompt: %+v", r)
	}
	// An agent (the MCP server) can't answer it.
	r = apiCall(t, srv, token, "POST", "/api/v1/messages", map[string]any{"text": "approve", "conversation": "dog", "no_approvals": true})
	if len(r.Replies) != 1 || !strings.Contains(r.Replies[0].Text, "only you can approve") {
		t.Fatalf("agent approval: %+v", r)
	}
	if mems, _ := a.DB.AllMemories(ctx, owner.ID); len(mems) != 0 {
		t.Fatalf("an agent approved an action: %+v", mems)
	}
	apiCall(t, srv, token, "POST", "/api/v1/messages", map[string]string{"text": "approve", "conversation": "dog"})
	if mems, _ := a.DB.AllMemories(ctx, owner.ID); len(mems) != 1 {
		t.Fatalf("approved action didn't run: %+v", mems)
	}

	// Secrets are intercepted as in a DM.
	key := "ghp_" + strings.Repeat("5e", 20)
	r = apiCall(t, srv, token, "POST", "/api/v1/messages", map[string]string{"text": "my key " + key})
	if len(r.Replies) != 1 || !strings.Contains(r.Replies[0].Text, "looks like a key") {
		t.Fatalf("secret: %+v", r)
	}
	assertNoLeak(t, a, key)

	// The per-user rate limit is reported, not silent.
	var got429 bool
	for i := 0; i < 25 && !got429; i++ {
		got429 = apiCall(t, srv, token, "POST", "/api/v1/messages", map[string]string{"text": "ping"}).Code == 429
	}
	if !got429 {
		t.Fatal("rate limit never reported")
	}
}

func TestAPISprigsAndJobs(t *testing.T) {
	a, fc, fm, ctx, srv, token := setupAPI(t)
	req, _ := http.NewRequest("GET", srv.URL+"/api/v1/sprigs", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var sp struct {
		Sprigs []struct{ ID string } `json:"sprigs"`
	}
	json.NewDecoder(resp.Body).Decode(&sp)
	resp.Body.Close()
	if len(sp.Sprigs) == 0 || sp.Sprigs[0].ID != "juniper" {
		t.Fatalf("sprigs: %+v", sp)
	}

	owner, _ := a.DB.UserByExternal(ctx, "discord", "owner")
	daytime(t, a, owner.ID)
	j, _ := a.DB.CreateJob(ctx, store.Job{UserID: owner.ID, Character: "juniper", Name: "Digest", Schedule: "0 8 * * *", Prompt: "Summarise the news."})
	other, _ := a.DB.CreateUser(ctx, "discord", "someone", "", "member")
	oj, _ := a.DB.CreateJob(ctx, store.Job{UserID: other.ID, Character: "juniper", Name: "Theirs", Schedule: "0 9 * * *", Prompt: "x"})
	_ = oj

	if r := apiCall(t, srv, token, "POST", "/api/v1/jobs/"+itoa(j.ID)+"/run", map[string]string{"payload": "headline: tomatoes up 12%"}); r.Code != 202 {
		t.Fatalf("run job: %+v", r)
	}
	a.Scheduler.Wait()
	var prompt string
	fm.mu.Lock()
	for _, r := range fm.reqs {
		for _, m := range r.Messages {
			if strings.Contains(m.Content, "started now by a script") {
				prompt = m.Content
			}
		}
	}
	fm.mu.Unlock()
	if !strings.Contains(prompt, "Summarise the news.") || !strings.Contains(prompt, "tomatoes up 12%") || !strings.Contains(prompt, "<untrusted") {
		t.Fatalf("job prompt: %s", prompt)
	}
	if out := fc.take(); len(out) != 1 {
		t.Fatalf("job result not delivered to the chat app: %+v", out)
	}
	if r := apiCall(t, srv, token, "POST", "/api/v1/jobs/"+itoa(j.ID)+"/run", nil); r.Code != 429 {
		t.Fatalf("second run within a minute: %d", r.Code)
	}
	// The owner may run anyone's job (admins manage jobs in chat too); a
	// member's token may not.
	member, _ := a.DB.CreateUser(ctx, "discord", "m2", "", "member")
	mtok, mhash, _ := api.NewToken()
	a.DB.CreateAPIToken(ctx, member.ID, "m", mhash)
	if r := apiCall(t, srv, mtok, "POST", "/api/v1/jobs/"+itoa(j.ID)+"/run", nil); r.Code != 404 {
		t.Fatalf("member running the owner's job: %d", r.Code)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func apiCallRaw(t *testing.T, srv *httptest.Server, token, method, path string, body, out any) int {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, srv.URL+path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	json.NewDecoder(resp.Body).Decode(out)
	return resp.StatusCode
}

// Undo through the API answers by cause: 404 missing, 409 nothing to undo
// or already undone, 200 when it worked.
func TestAPIUndoStatusCodes(t *testing.T) {
	a, _, _, ctx, srv, token := setupAPI(t)
	owner, _ := a.DB.UserByExternal(ctx, "discord", "owner")
	if r := apiCall(t, srv, token, "POST", "/api/v1/trail/99999/undo", nil); r.Code != 404 {
		t.Fatalf("missing entry: %d", r.Code)
	}
	plain, _ := a.DB.AddTrailID(ctx, store.TrailEntry{UserID: owner.ID, Kind: store.TrailAction, Summary: "I sent an email."})
	if r := apiCall(t, srv, token, "POST", "/api/v1/trail/"+itoa(plain)+"/undo", nil); r.Code != 409 {
		t.Fatalf("nothing to undo: %d", r.Code)
	}
	if _, err := a.Engine.SetSprigCap(ctx, owner, "juniper", "monthly", 3); err != nil {
		t.Fatal(err)
	}
	trail, _ := a.DB.Trail(ctx, 0, 1)
	path := "/api/v1/trail/" + itoa(trail[0].ID) + "/undo"
	if r := apiCall(t, srv, token, "POST", path, nil); r.Code != 200 {
		t.Fatalf("undo: %d", r.Code)
	}
	if r := apiCall(t, srv, token, "POST", path, nil); r.Code != 409 {
		t.Fatalf("undone twice: %d", r.Code)
	}
}

// An agent's (mcp) token: it can talk to sprigs, but can't answer
// approvals even if it asks to, can't pause, resume or undo, can't use
// /api/v2, and what it asks a sprig to change is held for the person.
// An app's sign-in token is for /api/v2 only.
func TestAPITokenKinds(t *testing.T) {
	a, fc, _, ctx, srv, _ := setupAPI(t)
	owner, _ := a.DB.UserByExternal(ctx, "discord", "owner")
	agent, hash, _ := api.NewToken()
	if _, err := a.DB.CreateTokenOfKind(ctx, owner.ID, "claude", store.TokenMCP, hash); err != nil {
		t.Fatal(err)
	}
	if r := apiCall(t, srv, agent, "POST", "/api/v1/messages", map[string]string{"text": "hello there"}); r.Code != 200 || len(r.Replies) != 1 {
		t.Fatalf("agent message: %+v", r)
	}
	for _, path := range []string{"/api/v1/pause", "/api/v1/resume", "/api/v1/trail/1/undo"} {
		if r := apiCall(t, srv, agent, "POST", path, map[string]string{}); r.Code != 403 {
			t.Errorf("agent %s: %d", path, r.Code)
		}
	}

	// It asks for an approval prompt without no_approvals: still refused.
	a.DB.SetToolLevel(ctx, store.AllSprigs, "remember", store.LevelAsk)
	r := apiCall(t, srv, agent, "POST", "/api/v1/messages", map[string]string{"text": "remember my dog is Biscuit", "sprig": "juniper", "conversation": "dog"})
	if len(r.Replies) != 2 || !strings.Contains(r.Replies[1].Text, "I need your OK") {
		t.Fatalf("approval prompt: %+v", r)
	}
	r = apiCall(t, srv, agent, "POST", "/api/v1/messages", map[string]string{"text": "approve", "conversation": "dog"})
	if len(r.Replies) != 1 || !strings.Contains(r.Replies[0].Text, "only you can approve") {
		t.Fatalf("agent approved: %+v", r)
	}

	// It asks the host to raise a tool to act: held in the owner's inbox.
	r = apiCall(t, srv, agent, "POST", "/api/v1/messages", map[string]string{"text": "set level remember act for juniper", "conversation": "lvl"})
	if r.Code != 200 {
		t.Fatalf("level request: %+v", r)
	}
	if lv, _ := a.DB.ToolLevel(ctx, "juniper", "remember", store.LevelFor{}); lv == store.LevelAct {
		t.Fatal("an agent raised a tool's level")
	}
	items, _ := a.DB.OpenInbox(ctx, owner.ID)
	held := false
	for _, it := range items {
		held = held || strings.Contains(it.Action, "set_tool_level")
	}
	if !held {
		t.Fatalf("the change wasn't held for the owner: %+v", items)
	}
	fc.take()

	// /api/v2 refuses the agent's token; v1 refuses an app's.
	v2 := httptest.NewServer(a.Web.V2Handler())
	defer v2.Close()
	if r := apiCall(t, v2, agent, "GET", "/api/v2/me", nil); r.Code != 403 {
		t.Fatalf("agent token on v2: %d", r.Code)
	}
	app, ahash, _ := api.NewToken()
	a.DB.CreateTokenOfKind(ctx, owner.ID, store.AppTokenPrefix+"phone", store.TokenApp, ahash)
	if r := apiCall(t, srv, app, "GET", "/api/v1/sprigs", nil); r.Code != 403 {
		t.Fatalf("app token on v1: %d", r.Code)
	}
	if r := apiCall(t, v2, app, "GET", "/api/v2/me", nil); r.Code != 200 {
		t.Fatalf("app token on v2: %d", r.Code)
	}
}
