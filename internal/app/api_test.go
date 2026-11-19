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
