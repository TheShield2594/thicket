// Package authtest is a fake OpenID Connect provider for tests.
package authtest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"time"
)

// Provider is a fake OIDC provider. Set Claims before a sign-in; the
// authorize endpoint "signs in" at once and redirects back with a code.
type Provider struct {
	*httptest.Server
	Key      *rsa.PrivateKey
	KID      string
	ClientID string
	Secret   string

	mu     sync.Mutex
	Claims map[string]any // merged into every ID token
	codes  map[string]grant
	// Tamper, if set, changes the ID token claims just before signing.
	Tamper func(map[string]any)
	// Exchanges counts successful token requests.
	Exchanges int
}

type grant struct {
	nonce, challenge, redirect string
}

// New starts a fake provider for one client.
func New(clientID, secret string) *Provider {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	p := &Provider{Key: key, KID: "k1", ClientID: clientID, Secret: secret, codes: map[string]grant{}, Claims: map[string]any{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"issuer": p.URL, "authorization_endpoint": p.URL + "/authorize", "token_endpoint": p.URL + "/token",
			"userinfo_endpoint": p.URL + "/userinfo", "jwks_uri": p.URL + "/jwks",
		})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, r *http.Request) {
		pub := p.Key.PublicKey
		json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{
			"kty": "RSA", "kid": p.KID, "use": "sig", "alg": "RS256",
			"n": b64(pub.N.Bytes()), "e": b64(big.NewInt(int64(pub.E)).Bytes()),
		}}})
	})
	mux.HandleFunc("GET /authorize", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("client_id") != p.ClientID || q.Get("code_challenge_method") != "S256" {
			http.Error(w, "bad request", 400)
			return
		}
		code := b64([]byte(time.Now().String()))
		p.mu.Lock()
		p.codes[code] = grant{q.Get("nonce"), q.Get("code_challenge"), q.Get("redirect_uri")}
		p.mu.Unlock()
		back, _ := url.Parse(q.Get("redirect_uri"))
		bq := back.Query()
		bq.Set("code", code)
		bq.Set("state", q.Get("state"))
		back.RawQuery = bq.Encode()
		http.Redirect(w, r, back.String(), http.StatusFound)
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		id, secret, ok := r.BasicAuth()
		r.ParseForm()
		p.mu.Lock()
		g, found := p.codes[r.PostFormValue("code")]
		delete(p.codes, r.PostFormValue("code"))
		p.mu.Unlock()
		sum := sha256.Sum256([]byte(r.PostFormValue("code_verifier")))
		switch {
		case !ok || id != p.ClientID || secret != p.Secret:
			w.WriteHeader(401)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid_client"})
			return
		case !found || g.redirect != r.PostFormValue("redirect_uri") || b64(sum[:]) != g.challenge:
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
			return
		}
		p.mu.Lock()
		p.Exchanges++
		p.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"id_token": p.Token(g.nonce), "access_token": "at", "token_type": "Bearer"})
	})
	mux.HandleFunc("GET /userinfo", func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"sub": p.Claims["sub"]})
	})
	p.Server = httptest.NewServer(mux)
	return p
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// Token makes a signed ID token with the current claims.
func (p *Provider) Token(nonce string) string {
	p.mu.Lock()
	claims := map[string]any{"iss": p.URL, "aud": p.ClientID, "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "nonce": nonce}
	for k, v := range p.Claims {
		claims[k] = v
	}
	tamper := p.Tamper
	p.mu.Unlock()
	if tamper != nil {
		tamper(claims)
	}
	return p.Sign(map[string]any{"alg": "RS256", "kid": p.KID, "typ": "JWT"}, claims)
}

// Sign makes a JWT with any header and claims, signed RS256 by the key.
func (p *Provider) Sign(header, claims map[string]any) string {
	h, _ := json.Marshal(header)
	c, _ := json.Marshal(claims)
	signed := b64(h) + "." + b64(c)
	sum := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, p.Key, crypto.SHA256, sum[:])
	if err != nil {
		panic(err)
	}
	return signed + "." + b64(sig)
}

// SetClaims replaces the claims the next sign-in gets.
func (p *Provider) SetClaims(c map[string]any) {
	p.mu.Lock()
	p.Claims = c
	p.mu.Unlock()
}
