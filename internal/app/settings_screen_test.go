package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/theshield2594/thicket/internal/agent"
	"github.com/theshield2594/thicket/internal/channel"
	"github.com/theshield2594/thicket/internal/store"
)

// The Settings screen changes what the host's chat tools change, for
// owners and admins only.
func TestSettingsScreen(t *testing.T) {
	a, _, _, ctx := setup(t)
	owner, err := a.DB.CreateUser(ctx, "web", "1", "Sam", store.RoleOwner)
	if err != nil {
		t.Fatal(err)
	}
	member, err := a.DB.CreateUser(ctx, "web", "2", "Kit", store.RoleMember)
	if err != nil {
		t.Fatal(err)
	}

	got, err := a.Engine.AdminSetSetting(ctx, owner, "model", "acme/big-1")
	if err != nil {
		t.Fatal(err)
	}
	if s := got.(agent.SettingInfo); s.Value != "acme/big-1" || !s.Changed || s.Default == "acme/big-1" {
		t.Fatalf("after set: %+v", s)
	}
	if v := a.Settings.String(ctx, "model"); v != "acme/big-1" {
		t.Fatalf("model is %q", v)
	}
	got, err = a.Engine.AdminSetSetting(ctx, owner, "model", "")
	if err != nil {
		t.Fatal(err)
	}
	if s := got.(agent.SettingInfo); s.Changed || s.Value != s.Default {
		t.Fatalf("after reset: %+v", s)
	}
	if _, err := a.Engine.AdminSetSetting(ctx, member, "model", "acme/big-1"); err == nil {
		t.Fatal("a member changed a setting")
	}
	if _, err := a.Engine.AdminSetSetting(ctx, owner, "nope", "1"); err == nil {
		t.Fatal("an unknown setting was accepted")
	}
	if _, err := a.Engine.AdminSetSetting(ctx, owner, "budget.daily_usd", "-1"); err == nil {
		t.Fatal("a negative cap was accepted")
	}

	// Connectors: the token goes to the encrypted store, never the server row.
	srv, _, _ := fakeHA(t)
	if _, err := a.Engine.AdminAddIntegration(ctx, member, "lights", srv.URL, ""); err == nil {
		t.Fatal("a member added a connector")
	}
	in, err := a.Engine.AdminAddIntegration(ctx, owner, "lights", srv.URL, "tok-abcdef123456")
	if err != nil {
		t.Fatal(err)
	}
	if c := in.(agent.IntegrationInfo); !strings.HasPrefix(c.Status, "connected") || len(c.Tools) == 0 {
		t.Fatalf("connector: %+v", c)
	}
	servers, _ := a.DB.MCPServers(ctx)
	if len(servers) != 1 || servers[0].Headers["Authorization"] != "Bearer {{secret:lights_token}}" {
		t.Fatalf("stored servers: %+v", servers)
	}
	if v, err := a.Secrets.Get(ctx, "lights_token"); err != nil || v != "tok-abcdef123456" {
		t.Fatalf("token secret: %q %v", v, err)
	}
	if _, err := a.Engine.AdminAddIntegration(ctx, owner, "lights", srv.URL, ""); err == nil {
		t.Fatal("a second connector with the same name was accepted")
	}
	if err := a.Engine.AdminRemoveIntegration(ctx, owner, "lights"); err != nil {
		t.Fatal(err)
	}

	// One that can't connect leaves nothing behind.
	if _, err := a.Engine.AdminAddIntegration(ctx, owner, "broken", "http://127.0.0.1:9/mcp", "tok-abcdef123456"); err == nil {
		t.Fatal("connecting to nothing worked")
	}
	if servers, _ := a.DB.MCPServers(ctx); len(servers) != 0 {
		t.Fatalf("left behind: %+v", servers)
	}
	if ok, _ := a.Secrets.Stored(ctx, "broken_token"); ok {
		t.Fatal("the token of a failed connector was kept")
	}
	// A token never replaces a secret that's already there, and a failed
	// attempt doesn't delete it.
	if err := a.Secrets.Put(ctx, "notes_token", "someone-elses-123456"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Engine.AdminAddIntegration(ctx, owner, "notes", srv.URL, "tok-abcdef123456"); err == nil {
		t.Fatal("a connector's token replaced an existing secret")
	}
	if v, _ := a.Secrets.Get(ctx, "notes_token"); v != "someone-elses-123456" {
		t.Fatalf("existing secret is now %q", v)
	}
	if servers, _ := a.DB.MCPServers(ctx); len(servers) != 0 {
		t.Fatalf("left behind: %+v", servers)
	}
	// A request that's cancelled while connecting still cleans up.
	gone, cancel := context.WithCancel(ctx)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cancel() // the person closed the tab while we were connecting
		http.Error(w, "busy", http.StatusServiceUnavailable)
	}))
	t.Cleanup(slow.Close)
	if _, err := a.Engine.AdminAddIntegration(gone, owner, "late", slow.URL, "tok-abcdef123456"); err == nil {
		t.Fatal("a cancelled request connected")
	}
	if ok, _ := a.Secrets.Stored(ctx, "late_token"); ok {
		t.Fatal("a cancelled request left its token stored")
	}
	if servers, _ := a.DB.MCPServers(ctx); len(servers) != 0 {
		t.Fatalf("a cancelled request left: %+v", servers)
	}
	// The same name set up from chat while a Settings add is connecting:
	// the failed add leaves the chat's connector and secret alone.
	fromChat := store.MCPServer{Name: "both", Transport: "http", Target: "http://chat.example/mcp", Enabled: true}
	busy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.Secrets.Put(ctx, "both_token", "from-chat-123456")
		a.DB.PutMCPServer(ctx, fromChat)
		http.Error(w, "busy", http.StatusServiceUnavailable)
	}))
	t.Cleanup(busy.Close)
	if _, err := a.Engine.AdminAddIntegration(ctx, owner, "both", busy.URL, "tok-abcdef123456"); err == nil {
		t.Fatal("connecting to a busy server worked")
	}
	if v, _ := a.Secrets.Get(ctx, "both_token"); v != "from-chat-123456" {
		t.Fatalf("the chat's secret is now %q", v)
	}
	if servers, _ := a.DB.MCPServers(ctx); len(servers) != 1 || servers[0].Target != fromChat.Target {
		t.Fatalf("the chat's connector: %+v", servers)
	}
	a.DB.DeleteMCPServer(ctx, "both")
	a.Secrets.Delete(ctx, "both_token")

	// Two people adding the same name at once: one wins, and the loser
	// neither replaces nor deletes the winner's token.
	type result struct {
		token string
		err   error
	}
	results := make(chan result, 2)
	for _, tok := range []string{"tok-first-123456", "tok-second-123456"} {
		go func() {
			_, err := a.Engine.AdminAddIntegration(ctx, owner, "twice", srv.URL, tok)
			results <- result{tok, err}
		}()
	}
	var won []string
	for range 2 {
		if r := <-results; r.err == nil {
			won = append(won, r.token)
		}
	}
	if len(won) != 1 {
		t.Fatalf("%d of two simultaneous adds succeeded, want 1", len(won))
	}
	if v, err := a.Secrets.Get(ctx, "twice_token"); err != nil || v != won[0] {
		t.Fatalf("token is %q (%v), want the winner's %q", v, err, won[0])
	}
	if err := a.Engine.AdminRemoveIntegration(ctx, owner, "twice"); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"Has Space", "a", "two__under"} {
		if _, err := a.Engine.AdminAddIntegration(ctx, owner, bad, srv.URL, ""); err == nil {
			t.Fatalf("name %q was accepted", bad)
		}
	}
	if _, err := a.Engine.AdminAddIntegration(ctx, owner, "files", "file:///etc/passwd", ""); err == nil {
		t.Fatal("a non-http address was accepted")
	}
}

// A preview speaks as a sprig that isn't in the roster yet, so the web
// app is told its name ("Wren (preview)") with the message.
func TestWebMessageKeepsPreviewName(t *testing.T) {
	a, _, _, ctx := setup(t)
	u, err := a.DB.CreateUser(ctx, "web", "1", "Sam", store.RoleOwner)
	if err != nil {
		t.Fatal(err)
	}
	host, _ := a.Chars.Host()
	chat := "1." + host.ID
	if u.ID != 1 {
		t.Fatalf("user id %d", u.ID)
	}
	if err := a.Web.Send(ctx, chat, channel.Persona{ID: host.ID, Name: host.DisplayName()}, "hello"); err != nil {
		t.Fatal(err)
	}
	if err := a.Web.Send(ctx, chat, channel.Persona{ID: "wren", Name: "Wren (preview)"}, "hi, I'm Wren"); err != nil {
		t.Fatal(err)
	}
	msgs, err := a.DB.WebMessages(ctx, chat, 0, 10)
	if err != nil || len(msgs) != 2 {
		t.Fatalf("messages: %v %v", msgs, err)
	}
	if msgs[0].Name != "" || msgs[1].Name != "Wren (preview)" || msgs[1].Character != "wren" {
		t.Fatalf("names: %+v", msgs)
	}
}
