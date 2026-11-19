package app

import (
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
