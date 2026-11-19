package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/theshield2594/thicket/internal/auth"
	"github.com/theshield2594/thicket/internal/store"
)

type v2client struct {
	t     *testing.T
	base  string
	token string
}

func (c *v2client) call(method, path string, body any) (int, map[string]any) {
	c.t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, c.base+path, rd)
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer res.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func TestAppAPIv2(t *testing.T) {
	h := setupHousehold(t)
	a, ctx := h.a, h.ctx
	for u, name := range map[*store.User]string{h.owner: "sam", h.kim: "kim", h.kid: "max"} {
		hash, _ := auth.HashPassword(name + " long password")
		if err := a.DB.SetLogin(ctx, u.ID, name, hash); err != nil {
			t.Fatal(err)
		}
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	go a.Web.Run(runCtx, a.Engine.HandleIncoming)
	waitFor(t, "web adapter", a.Web.Connected)
	srv := httptest.NewServer(a.Web.V2Handler())
	defer srv.Close()

	anon := &v2client{t: t, base: srv.URL}
	if code, info := anon.call("GET", "/api/v2", nil); code != 200 || info["api"] != "2.0" {
		t.Fatalf("discovery: %d %v", code, info)
	}
	if code, _ := anon.call("GET", "/api/v2/me", nil); code != 401 {
		t.Fatalf("no token: %d", code)
	}
	login := func(user string) *v2client {
		t.Helper()
		code, out := anon.call("POST", "/api/v2/auth/login", map[string]string{"username": user, "password": user + " long password", "device": "iPhone"})
		if code != 200 {
			t.Fatalf("login %s: %d %v", user, code, out)
		}
		return &v2client{t: t, base: srv.URL, token: out["token"].(string)}
	}
	if code, _ := anon.call("POST", "/api/v2/auth/login", map[string]string{"username": "sam", "password": "nope"}); code != 401 {
		t.Fatalf("wrong password: %d", code)
	}
	owner, kim, kid := login("sam"), login("kim"), login("max")

	// Who sees which sprigs.
	sprigIDs := func(c *v2client) string {
		_, me := c.call("GET", "/api/v2/me", nil)
		var ids []string
		for _, s := range me["sprigs"].([]any) {
			ids = append(ids, s.(map[string]any)["id"].(string))
		}
		return strings.Join(ids, ",")
	}
	if got := sprigIDs(kid); got != "bramble" {
		t.Fatalf("kid's sprigs: %s", got)
	}
	if got := sprigIDs(kim); strings.Contains(got, "bramble") || !strings.Contains(got, "juniper") {
		t.Fatalf("Kim's sprigs: %s", got)
	}

	// Kid boundaries over the API.
	for _, p := range []string{"/api/v2/calendar", "/api/v2/calendars", "/api/v2/household", "/api/v2/trail", "/api/v2/kid-sprigs/bramble/chats"} {
		if code, _ := kid.call("GET", p, nil); code != 403 {
			t.Errorf("kid GET %s: %d", p, code)
		}
	}
	if code, _ := kid.call("PATCH", fmt.Sprintf("/api/v2/household/members/%d", h.kid.ID), map[string]string{"role": "admin"}); code != 400 {
		t.Fatalf("kid made themselves admin: %d", code)
	}
	if code, _ := kid.call("PATCH", "/api/v2/kid-sprigs/bramble/access", map[string]any{"add_allow": []string{"calendar"}}); code != 400 {
		t.Fatalf("kid widened their own sprig: %d", code)
	}

	// Parents read kid chats; other adults don't.
	h.kidSays("hello bramble")
	if code, out := owner.call("GET", "/api/v2/kid-sprigs/bramble/chats", nil); code != 200 || len(out["messages"].([]any)) == 0 {
		t.Fatalf("parent reading kid chats: %d %v", code, out)
	}
	if code, _ := kim.call("GET", "/api/v2/kid-sprigs/bramble/chats", nil); code != 403 {
		t.Fatalf("non-parent read kid chats: %d", code)
	}

	// Chat and approvals: the same threads as the web app.
	chat := fmt.Sprintf("/api/v2/chats/%d.hal/messages", h.owner.ID)
	if code, out := owner.call("POST", chat, map[string]string{"text": "turn on the light"}); code != 200 {
		t.Fatalf("post: %d %v", code, out)
	}
	var approvalID float64
	waitFor(t, "an approval", func() bool {
		_, out := owner.call("GET", "/api/v2/approvals", nil)
		aps, _ := out["approvals"].([]any)
		if len(aps) == 1 {
			approvalID = aps[0].(map[string]any)["id"].(float64)
		}
		return len(aps) == 1
	})
	if code, _ := kim.call("POST", fmt.Sprintf("/api/v2/approvals/%d", int64(approvalID)), map[string]bool{"approve": true}); code != 400 {
		t.Fatalf("someone else answered the approval: %d", code)
	}
	if code, out := owner.call("POST", fmt.Sprintf("/api/v2/approvals/%d", int64(approvalID)), map[string]bool{"approve": true}); code != 200 {
		t.Fatalf("approve: %d %v", code, out)
	}
	waitFor(t, "the approved action", func() bool { return h.writes() == 1 })
	if code, out := owner.call("GET", chat, nil); code != 200 || len(out["messages"].([]any)) < 2 {
		t.Fatalf("history: %d %v", code, out)
	}

	// Household admin: invites, roles (in the trail, undoable), kid sprigs, levels.
	if code, _ := kim.call("POST", "/api/v2/household/invites", map[string]string{"name": "Ana", "role": "kid"}); code != 403 {
		t.Fatalf("member invited someone: %d", code)
	}
	if code, out := owner.call("POST", "/api/v2/household/invites", map[string]string{"name": "Ana", "role": "kid"}); code != 201 || !strings.Contains(out["invite_url"].(string), "/invite#") {
		t.Fatalf("invite: %d %v", code, out)
	}
	if code, _ := owner.call("PATCH", fmt.Sprintf("/api/v2/household/members/%d", h.owner.ID), map[string]string{"role": "member"}); code != 400 {
		t.Fatalf("owner demoted: %d", code)
	}
	if code, _ := owner.call("PATCH", fmt.Sprintf("/api/v2/household/members/%d", h.kim.ID), map[string]string{"role": "admin"}); code != 200 {
		t.Fatalf("role change: %d", code)
	}
	_, tr := owner.call("GET", "/api/v2/trail", nil)
	first := tr["trail"].([]any)[0].(map[string]any)
	if first["kind"] != "role" || first["undoable"] != true {
		t.Fatalf("trail: %v", first)
	}
	if code, _ := owner.call("POST", fmt.Sprintf("/api/v2/trail/%d/undo", int64(first["id"].(float64))), nil); code != 200 {
		t.Fatalf("undo role: %d", code)
	}
	if u, _ := a.DB.UserByID(ctx, h.kim.ID); u.Role != store.RoleMember {
		t.Fatalf("role after undo: %s", u.Role)
	}
	spec := map[string]any{"id": "pebble", "name": "Pebble", "role": "A homework buddy.", "kids": []int64{h.kid.ID}, "parents": []int64{h.kim.ID}}
	if code, _ := kim.call("POST", "/api/v2/kid-sprigs", spec); code != 400 {
		t.Fatalf("member created a kid sprig: %d", code)
	}
	if code, _ := owner.call("POST", "/api/v2/kid-sprigs", map[string]any{"id": "pebble", "name": "Pebble", "role": "x", "kids": []int64{h.kim.ID}, "parents": []int64{h.owner.ID}}); code != 400 {
		t.Fatalf("an adult was made a kid sprig's kid: %d", code)
	}
	if code, out := owner.call("POST", "/api/v2/kid-sprigs", spec); code != 201 {
		t.Fatalf("create kid sprig: %d %v", code, out)
	}
	if code, out := kim.call("PATCH", "/api/v2/kid-sprigs/pebble/access", map[string]any{"add_tools": []string{"web"}}); code != 200 || fmt.Sprint(out["tools"]) != "[web]" {
		t.Fatalf("parent approving a tool: %d %v", code, out)
	}
	if code, _ := kim.call("PUT", "/api/v2/levels", map[string]string{"sprig": "hal", "tool": "mcp_ha__turn_on", "level": "act"}); code != 400 {
		t.Fatalf("member changed a level: %d", code)
	}
	if code, _ := owner.call("PUT", "/api/v2/levels", map[string]string{"sprig": "pebble", "tool": "web_search", "level": "act"}); code != 400 {
		t.Fatalf("kid sprig raised to act: %d", code)
	}

	// Signing out revokes the token.
	if code, _ := kim.call("POST", "/api/v2/auth/logout", nil); code != 200 {
		t.Fatal("logout")
	}
	if code, _ := kim.call("GET", "/api/v2/me", nil); code != 401 {
		t.Fatalf("revoked token still works: %d", code)
	}
}
