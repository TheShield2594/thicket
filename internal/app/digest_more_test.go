package app

import (
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/theshield2594/thicket/internal/auth"
	"github.com/theshield2594/thicket/internal/store"
)

// A digest due inside the host's quiet hours (Juni's are 22:00-08:00) is
// held until they end, then sent once, still within its window.
func TestWeeklyDigestWaitsOutQuietHours(t *testing.T) {
	h := setupHousehold(t)
	a, ctx, fc := h.a, h.ctx, h.fc
	if err := a.DB.UpdateUserProfile(ctx, h.owner.ID, "", "Etc/GMT"); err != nil {
		t.Fatal(err)
	}
	if err := a.DB.SetDigestMode(ctx, h.kim.ID, store.DigestOff); err != nil {
		t.Fatal(err)
	}
	fc.take()
	// Due at 23:00 UTC two days from now (after everyone joined).
	d := time.Now().UTC().Add(48 * time.Hour)
	due := time.Date(d.Year(), d.Month(), d.Day(), 23, 0, 0, 0, time.UTC)
	if err := a.Settings.Set(ctx, "digest.day", strings.ToLower(due.Weekday().String())); err != nil {
		t.Fatal(err)
	}
	if err := a.Settings.Set(ctx, "digest.time", "23:00"); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	clock := due
	a.Engine.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	at := func(t time.Time) { mu.Lock(); clock = t; mu.Unlock() }

	// Through the night: held.
	for _, after := range []time.Duration{10 * time.Minute, 3 * time.Hour, 8*time.Hour + 59*time.Minute} {
		at(due.Add(after))
		a.Engine.WeeklyDigests(ctx)
		if out := fc.take(); len(out) != 0 {
			t.Fatalf("sent during quiet hours (%s after it was due): %+v", after, out)
		}
	}
	// 08:05: quiet hours are over, and it's well inside the 24-hour window.
	at(due.Add(9*time.Hour + 5*time.Minute))
	a.Engine.WeeklyDigests(ctx)
	out := fc.take()
	if len(out) != 1 || out[0].chat != "dm-owner" || !strings.Contains(out[0].text, "**Your week**") {
		t.Fatalf("after quiet hours: %+v", out)
	}
	// Not twice: later that day, or into the next night.
	for _, after := range []time.Duration{10 * time.Hour, 20 * time.Hour, 23 * time.Hour} {
		at(due.Add(after))
		a.Engine.WeeklyDigests(ctx)
		if out := fc.take(); len(out) != 0 {
			t.Fatalf("sent again %s after it was due: %+v", after, out)
		}
	}
}

// Each adult reads and changes their own weekly digest choice over API
// v2; nobody can change anyone else's, and kids have none.
func TestDigestPrefAPI(t *testing.T) {
	h := setupHousehold(t)
	a, ctx := h.a, h.ctx
	for u, name := range map[*store.User]string{h.owner: "sam", h.kim: "kim", h.kid: "max"} {
		hash, _ := auth.HashPassword(name + " long password")
		if err := a.DB.SetLogin(ctx, u.ID, name, hash); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Settings.Set(ctx, "digest.day", "saturday"); err != nil {
		t.Fatal(err)
	}
	if err := a.Settings.Set(ctx, "digest.time", "09:30"); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a.Web.V2Handler())
	defer srv.Close()
	anon := &v2client{t: t, base: srv.URL}
	login := func(user string) *v2client {
		t.Helper()
		code, out := anon.call("POST", "/api/v2/auth/login", map[string]string{"username": user, "password": user + " long password", "device": "iPhone"})
		if code != 200 {
			t.Fatalf("login %s: %d %v", user, code, out)
		}
		return &v2client{t: t, base: srv.URL, token: out["token"].(string)}
	}
	owner, kim, kid := login("sam"), login("kim"), login("max")

	if code, _ := anon.call("GET", "/api/v2/digest", nil); code != 401 {
		t.Fatalf("no token: %d", code)
	}
	code, got := kim.call("GET", "/api/v2/digest", nil)
	if code != 200 || got["mode"] != "always" || got["day"] != "saturday" || got["time"] != "09:30" {
		t.Fatalf("Kim's digest: %d %v", code, got)
	}
	if code, got := kim.call("PUT", "/api/v2/digest", map[string]string{"mode": "skip_empty"}); code != 200 || got["mode"] != "skip_empty" {
		t.Fatalf("set Kim's: %d %v", code, got)
	}
	if m, _ := a.DB.DigestMode(ctx, h.kim.ID); m != store.DigestSkipEmpty {
		t.Fatalf("stored: %q", m)
	}
	// Only her own: the owner's still reads as always.
	if code, got := owner.call("GET", "/api/v2/digest", nil); code != 200 || got["mode"] != "always" {
		t.Fatalf("owner's digest: %d %v", code, got)
	}
	// There's no naming someone else: a user_id in the body is ignored,
	// even from an admin, and only the caller's own choice changes.
	if code, _ := owner.call("PUT", "/api/v2/digest", map[string]any{"mode": "off", "user_id": h.kim.ID}); code != 200 {
		t.Fatalf("owner set theirs: %d", code)
	}
	if m, _ := a.DB.DigestMode(ctx, h.kim.ID); m != store.DigestSkipEmpty {
		t.Fatalf("the owner changed Kim's: %q", m)
	}
	if m, _ := a.DB.DigestMode(ctx, h.owner.ID); m != store.DigestOff {
		t.Fatalf("owner's own: %q", m)
	}
	if code, _ := kim.call("PUT", "/api/v2/digest", map[string]string{"mode": "daily"}); code != 400 {
		t.Fatalf("bad mode: %d", code)
	}
	// Kids get no digest.
	if code, _ := kid.call("GET", "/api/v2/digest", nil); code != 403 {
		t.Fatalf("kid GET: %d", code)
	}
	if code, _ := kid.call("PUT", "/api/v2/digest", map[string]string{"mode": "always"}); code != 403 {
		t.Fatalf("kid PUT: %d", code)
	}
	// The household switching it off shows as day "off"; her choice stays.
	if err := a.Settings.Set(ctx, "digest.day", "off"); err != nil {
		t.Fatal(err)
	}
	if _, got := kim.call("GET", "/api/v2/digest", nil); got["day"] != "off" || got["mode"] != "skip_empty" {
		t.Fatalf("household off: %v", got)
	}
}
