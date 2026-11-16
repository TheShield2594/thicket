package auth_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/theshield2594/thicket/internal/auth"
	"github.com/theshield2594/thicket/internal/auth/authtest"
)

func TestPasswords(t *testing.T) {
	h, err := auth.HashPassword("correct horse battery")
	if err != nil || !strings.HasPrefix(h, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("hash: %q %v", h, err)
	}
	if h2, _ := auth.HashPassword("correct horse battery"); h2 == h {
		t.Fatal("no salt")
	}
	if auth.VerifyPassword(h, "correct horse battery") != nil {
		t.Fatal("right password refused")
	}
	for _, bad := range []string{"correct horse batterY", "", strings.Repeat("x", 300)} {
		if auth.VerifyPassword(h, bad) == nil {
			t.Fatalf("wrong password %q accepted", bad)
		}
	}
	// A planted hash with huge parameters is refused, not run.
	if auth.VerifyPassword("$argon2id$v=19$m=99999999,t=1,p=1$c2FsdHNhbHQ$a2V5a2V5a2V5a2V5a2V5a2V5", "x") == nil {
		t.Fatal("absurd parameters accepted")
	}
	if auth.VerifyPassword("", "anything") == nil {
		t.Fatal("empty hash matched")
	}
	for pw, ok := range map[string]bool{"short": false, "ten chars!": true, " padded password": false} {
		if (auth.CheckPasswordRules(pw) == nil) != ok {
			t.Errorf("rules for %q", pw)
		}
	}
}

func provider(t *testing.T) (*authtest.Provider, *auth.Provider) {
	t.Helper()
	fake := authtest.New("thicket", "s3cret")
	t.Cleanup(fake.Close)
	p := auth.NewProvider(auth.OIDCConfig{Issuer: fake.URL, ClientID: "thicket", ClientSecret: "s3cret", RedirectURL: "https://thicket.example/auth/oidc/callback"})
	return fake, p
}

func TestOIDCFlow(t *testing.T) {
	fake, p := provider(t)
	ctx := context.Background()
	fake.SetClaims(map[string]any{"sub": "u-1", "email": "kim@example.com", "email_verified": true, "name": "Kim", "groups": []any{"family"}})
	verifier, nonce := auth.Random(), auth.Random()
	u, err := p.AuthURL(ctx, "st", nonce, verifier)
	if err != nil || !strings.Contains(u, "code_challenge="+auth.Challenge(verifier)) || !strings.Contains(u, "scope=openid+email+profile") {
		t.Fatalf("auth url: %s %v", u, err)
	}
	// Ask the fake to "sign in" and take the code from its redirect.
	resp, err := noRedirect().Get(u)
	if err != nil {
		t.Fatal(err)
	}
	loc, _ := resp.Location()
	code := loc.Query().Get("code")
	if loc.Query().Get("state") != "st" || code == "" {
		t.Fatalf("redirect: %s", loc)
	}
	if _, err := p.Exchange(ctx, code, "wrong verifier", nonce); err == nil {
		t.Fatal("PKCE not checked")
	}
	resp, _ = noRedirect().Get(u)
	loc, _ = resp.Location()
	cl, err := p.Exchange(ctx, loc.Query().Get("code"), verifier, nonce)
	if err != nil {
		t.Fatal(err)
	}
	if cl.Subject != "u-1" || cl.Email != "kim@example.com" || !cl.EmailVerified || cl.Name != "Kim" || cl.Strings("groups")[0] != "family" {
		t.Fatalf("claims: %+v", cl)
	}
}

func TestIDTokenChecks(t *testing.T) {
	fake, p := provider(t)
	ctx := context.Background()
	fake.SetClaims(map[string]any{"sub": "u-1"})
	if _, err := p.Verify(ctx, fake.Token("n1"), "n1"); err != nil {
		t.Fatalf("good token refused: %v", err)
	}
	cases := map[string]func(map[string]any){
		"wrong audience": func(c map[string]any) { c["aud"] = "someone-else" },
		"expired":        func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() },
		"wrong issuer":   func(c map[string]any) { c["iss"] = "https://evil.example" },
		"no subject":     func(c map[string]any) { delete(c, "sub") },
		"azp mismatch":   func(c map[string]any) { c["aud"] = []any{"thicket", "other"}; c["azp"] = "other" },
	}
	for name, tamper := range cases {
		fake.Tamper = tamper
		if _, err := p.Verify(ctx, fake.Token("n1"), "n1"); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	fake.Tamper = nil
	if _, err := p.Verify(ctx, fake.Token("n1"), "n2"); err == nil {
		t.Error("wrong nonce accepted")
	}
	tok := fake.Token("n1")
	if _, err := p.Verify(ctx, tok[:len(tok)-4]+"AAAA", "n1"); err == nil {
		t.Error("bad signature accepted")
	}
	// alg "none" and HMAC (signed with something public) are refused.
	parts := strings.Split(tok, ".")
	if _, err := p.Verify(ctx, "eyJhbGciOiJub25lIn0."+parts[1]+".", "n1"); err == nil {
		t.Error(`alg "none" accepted`)
	}
	hs := fake.Sign(map[string]any{"alg": "HS256", "kid": "k1"}, map[string]any{"sub": "x"})
	if _, err := p.Verify(ctx, hs, "n1"); err == nil || !strings.Contains(err.Error(), "doesn't accept") {
		t.Errorf("HS256 accepted: %v", err)
	}
}

func TestKeyRotationRefetchesKeys(t *testing.T) {
	fake := authtest.New("thicket", "s3cret")
	defer fake.Close()
	now := time.Now()
	p := auth.NewProvider(auth.OIDCConfig{Issuer: fake.URL, ClientID: "thicket", ClientSecret: "s3cret",
		Now: func() time.Time { return now }})
	ctx := context.Background()
	fake.SetClaims(map[string]any{"sub": "u-1"})
	if _, err := p.Verify(ctx, fake.Token("n"), "n"); err != nil {
		t.Fatal(err)
	}
	fake.Key, _ = rsa.GenerateKey(rand.Reader, 2048)
	fake.KID = "k2"
	// Within a minute of the last fetch, an unknown key isn't refetched.
	if _, err := p.Verify(ctx, fake.Token("n"), "n"); err == nil || !strings.Contains(err.Error(), "unknown key") {
		t.Fatalf("unknown key: %v", err)
	}
	now = now.Add(2 * time.Minute)
	if _, err := p.Verify(ctx, fake.Token("n"), "n"); err != nil {
		t.Fatalf("rotated key not picked up: %v", err)
	}
}

func noRedirect() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
