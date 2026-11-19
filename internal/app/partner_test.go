package app

// "Add my partner's sprig": the owner asks Juni for an invite, the partner
// arrives from it and is greeted and helped to a sprig of their own, and
// that sprig is private: the owner's dashboard, API and trail show only
// that it exists, what it costs and whether it's working.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/theshield2594/thicket/internal/api"
	"github.com/theshield2594/thicket/internal/channel"
	"github.com/theshield2594/thicket/internal/store"
	"github.com/theshield2594/thicket/internal/web"
)

func partnerDM(user, text string) channel.Incoming {
	return channel.Incoming{Channel: "discord", ChatID: "dm-" + user, MessageID: "m-" + text, UserID: user, Text: text, IsDM: true}
}

// chatRequest finds the newest model request whose messages mention s.
func chatRequest(fm *fakeModel, s string) (string, bool) {
	fm.mu.Lock()
	defer fm.mu.Unlock()
	for i := len(fm.reqs) - 1; i >= 0; i-- {
		for _, m := range fm.reqs[i].Messages[1:] {
			if strings.Contains(m.Content, s) {
				return fm.reqs[i].Messages[0].Content, true
			}
		}
	}
	return "", false
}

func TestAddPartnersPrivateSprig(t *testing.T) {
	a, fc, fm, ctx := setup(t)
	onboard(t, a, fc, ctx)
	owner, _ := a.DB.UserByExternal(ctx, "discord", "owner")
	_ = a.DB.UpdateUserProfile(ctx, owner.ID, "Sam", "")

	// 1. The owner asks; the link is posted by code, after Juni's reply.
	a.Engine.HandleIncoming(ctx, dm("invite my partner Robin on web"))
	out := fc.take()
	link := ""
	for _, s := range out {
		if i := strings.Index(s.text, "/invite#"); i >= 0 {
			link = strings.Fields(s.text[i+len("/invite#"):])[0]
		}
	}
	if len(link) != 43 {
		t.Fatalf("no invite link posted: %+v", out)
	}
	if res := lastToolResult(fm); strings.Contains(res, link) || !strings.Contains(res, "private") || !strings.Contains(res, "database file") {
		t.Fatalf("Juni should get the privacy note but never the link: %q", res)
	}

	// 2. Robin uses it from Discord: Juni greets Robin, not Sam.
	a.Engine.HandleIncoming(ctx, partnerDM("robin", "join-"+link))
	out = fc.take()
	var greeted bool
	for _, s := range out {
		if s.chat == "dm-owner" {
			t.Fatalf("the owner was messaged: %+v", s)
		}
		if s.chat == "dm-robin" && s.persona == "Juni" {
			greeted = true
		}
	}
	if !greeted {
		t.Fatalf("Robin wasn't greeted: %+v", out)
	}
	robin, err := a.DB.UserByExternal(ctx, "discord", "robin")
	if err != nil {
		t.Fatal(err)
	}
	if robin.Name != "Robin" || robin.Role != store.RoleMember || !robin.OwnSprig || robin.InvitedBy != owner.ID {
		t.Fatalf("Robin's account: %+v", robin)
	}
	sys, ok := chatRequest(fm, "invite from Sam")
	if !ok || !strings.Contains(sys, "Welcome tour") || !strings.Contains(sys, "invited to make a sprig of their own") || strings.Contains(sys, "First-run onboarding") {
		t.Fatalf("the welcome should start Robin's tour, with their own sprig first:\n%s", sys)
	}
	// The code is single use, and was taken out of the chat.
	a.Engine.HandleIncoming(ctx, partnerDM("someone", "join-"+link))
	if got := sentText(fc.take()); !strings.Contains(got, "expired or was already used") {
		t.Fatalf("second use: %q", got)
	}
	if _, err := a.DB.UserByExternal(ctx, "discord", "someone"); err == nil {
		t.Fatal("a used invite made another account")
	}

	// 3. Robin makes a private sprig; only Robin sees the approval.
	a.Engine.HandleIncoming(ctx, partnerDM("robin", "bring on my own wren"))
	a.Engine.HandleIncoming(ctx, partnerDM("robin", "create wren"))
	for _, s := range fc.take() {
		if s.chat != "dm-robin" {
			t.Fatalf("the interview reached someone else: %+v", s)
		}
	}
	a.Engine.HandleIncoming(ctx, partnerDM("robin", "approve"))
	fc.take()
	wren, ok := a.Chars.Get("wren")
	if !ok || wren.BelongsTo != robin.ID {
		t.Fatalf("Robin's sprig: %+v", wren)
	}
	a.DB.LogActivity(ctx, store.Activity{Level: "info", Kind: "tool", Character: "wren", UserID: robin.ID, Summary: "SECRET-ACTIVITY"})
	a.DB.LogActivity(ctx, store.Activity{Level: "error", Kind: "model", Character: "wren", UserID: robin.ID, Summary: "SECRET-ERROR"})
	a.DB.RecordUsage(ctx, store.Usage{UserID: robin.ID, Character: "wren", Origin: "task", Model: "m", CostUSD: 0.42})

	// Nothing about the draft reached the activity log, details included.
	acts, _ := a.DB.RecentActivity(ctx, 200, "info")
	for _, x := range acts {
		if strings.Contains(x.Detail+x.Summary, "Wren") && x.Character != "wren" {
			t.Fatalf("activity outside Wren's own rows names it: %+v", x)
		}
	}
	// An admin can't undo changes to Robin's private sprig either.
	if all, _ := a.DB.Trail(ctx, 0, 50); len(all) > 0 {
		for _, e := range all {
			if e.Character == "wren" && e.Undoable {
				if _, err := a.Engine.UndoTrail(ctx, owner, e.ID); err == nil {
					t.Fatal("the owner undid a change to Robin's private sprig")
				}
			}
		}
	}

	secrets := []string{"Wren", "wren", "secret reading list", "SECRET-"}
	noSecrets := func(where, body string) {
		t.Helper()
		for _, s := range secrets {
			if strings.Contains(body, s) {
				t.Errorf("%s shows %q", where, s)
			}
		}
	}

	// 4. The owner's dashboard: that it exists, its cost and health only.
	srv := httptest.NewServer(a.Dashboard.Handler())
	defer srv.Close()
	session, _ := a.DB.CreateWebSession(ctx, owner.ID, "test", web.SessionTTL)
	_, body := getDashboard(t, srv, session)
	for _, want := range []string{"Robin&#39;s private sprig", "1 error(s) in the last day", "$0.42"} {
		if !strings.Contains(body, want) {
			t.Errorf("owner's dashboard missing %q", want)
		}
	}
	noSecrets("owner's dashboard", body)
	req, _ := http.NewRequest("GET", srv.URL+"/dashboard/state.json", nil)
	req.AddCookie(&http.Cookie{Name: "thicket_session", Value: session})
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("state.json: %d %v", res.StatusCode, err)
	}
	noSecrets("state.json", string(b))
	// Robin's own dashboard does show it.
	rs, _ := a.DB.CreateWebSession(ctx, robin.ID, "test", web.SessionTTL)
	if _, rbody := getDashboard(t, srv, rs); !strings.Contains(rbody, "Wren") {
		t.Error("Robin's dashboard should show Wren")
	}

	// 5. The owner's app API: trail and household.
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	go a.Web.Run(runCtx, a.Engine.HandleIncoming)
	waitFor(t, "web adapter", a.Web.Connected)
	wsrv := httptest.NewServer(a.Web.V2Handler())
	defer wsrv.Close()
	tok, hash, _ := api.NewToken()
	if _, err := a.DB.CreateAPIToken(ctx, owner.ID, "test", hash); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v2/trail", "/api/v2/household", "/api/v2/me"} {
		req, _ := http.NewRequest("GET", wsrv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("%s: %d %s", path, res.StatusCode, b)
		}
		noSecrets(path, string(b))
		if path == "/api/v2/household" {
			var hh struct {
				Sprigs []map[string]any `json:"sprigs"`
			}
			json.Unmarshal(b, &hh)
			found := false
			for _, s := range hh.Sprigs {
				found = found || (s["private"] == true && s["belongs_to"] == float64(robin.ID))
			}
			if !found {
				t.Errorf("the household list should say Robin has a private sprig: %s", b)
			}
		}
	}
	// The raw trail does hold Wren's entries: the API hid them.
	if all, _ := a.DB.Trail(ctx, 0, 50); !func() bool {
		for _, e := range all {
			if e.Character == "wren" {
				return true
			}
		}
		return false
	}() {
		t.Fatal("expected a trail entry for Wren to hide")
	}

	// 6. If Wren's file goes away, its history stays hidden.
	if err := os.Remove(filepath.Join(a.Cfg.CharactersDir, "wren.md")); err != nil {
		t.Fatal(err)
	}
	if err := a.Chars.Load(); err != nil {
		t.Fatal(err)
	}
	_, body = getDashboard(t, srv, session)
	noSecrets("owner's dashboard after Wren's file was removed", body)

	// 7. A private sprig is never previewed in a group channel.
	group := partnerDM("robin", "bring on my own wren")
	group.ChatID, group.IsDM, group.Mentioned = "group-1", false, true
	a.Engine.HandleIncoming(ctx, group)
	for _, s := range fc.take() {
		if strings.Contains(s.text, "secret reading list") || strings.Contains(s.persona, "preview") {
			t.Fatalf("a private preview reached the group: %+v", s)
		}
	}
	if res := lastToolResult(fm); !strings.Contains(res, "previewed in a DM") {
		t.Fatalf("group preview: %q", res)
	}

	// 8. Nor can the owner change it from chat.
	if err := a.Engine.SetToolLevel(ctx, owner, "wren", "web", store.LevelAct); err == nil {
		t.Error("the owner changed a level on Robin's private sprig")
	}
}

// Someone already on Thicket gets nothing from a join code.
func TestJoinCodeForKnownAccount(t *testing.T) {
	a, fc, _, ctx := setup(t)
	onboard(t, a, fc, ctx)
	tok, _ := a.DB.CreateInvite(ctx, store.Invite{Role: store.RoleMember, Name: "X"}, time.Hour)
	a.Engine.HandleIncoming(ctx, dm("join-"+tok))
	if got := sentText(fc.take()); !strings.Contains(got, "already set up") {
		t.Fatalf("owner sending a join code: %q", got)
	}
	if inv, err := a.DB.InviteByToken(ctx, tok); err != nil || inv == nil {
		t.Fatal("the invite should still be usable")
	}
}
