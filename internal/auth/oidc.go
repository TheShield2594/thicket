package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	_ "crypto/sha512" // SHA-384 and SHA-512 for RS384/RS512/ES384
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// OIDCConfig is what Thicket needs to sign people in with an OpenID
// Connect provider (Authentik, Authelia, Keycloak, Google...).
type OIDCConfig struct {
	Issuer       string
	ClientID     string
	ClientSecret string
	RedirectURL  string   // <public url>/auth/oidc/callback
	Scopes       []string // default: openid email profile
	// Wanted are claims (a groups claim, say) worth a userinfo call when
	// the ID token lacks them. email always is.
	Wanted []string
	HTTP   *http.Client
	Now    func() time.Time
}

// Provider is an OIDC provider, discovered on first use. It's safe for
// concurrent use.
type Provider struct {
	cfg OIDCConfig

	mu     sync.Mutex
	meta   *discovery
	metaAt time.Time
	keys   map[string]jwk
	keysAt time.Time
}

type discovery struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	UserinfoEndpoint      string   `json:"userinfo_endpoint"`
	JWKSURI               string   `json:"jwks_uri"`
	TokenAuthMethods      []string `json:"token_endpoint_auth_methods_supported"`
}

type jwk struct {
	kid string
	kty string
	key crypto.PublicKey
}

// Claims are the ID token claims Thicket uses, plus all of them raw (for
// a configurable groups claim).
type Claims struct {
	Subject       string
	Email         string
	EmailVerified bool
	Name          string
	Username      string
	Raw           map[string]any
}

// Strings returns a claim as a list of strings (a string or an array).
func (c *Claims) Strings(name string) []string {
	switch v := c.Raw[name].(type) {
	case string:
		return []string{v}
	case []any:
		var out []string
		for _, x := range v {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func NewProvider(cfg OIDCConfig) *Provider {
	cfg.Issuer = strings.TrimRight(cfg.Issuer, "/")
	if len(cfg.Scopes) == 0 {
		cfg.Scopes = []string{"openid", "email", "profile"}
	}
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 15 * time.Second}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Provider{cfg: cfg}
}

// Issuer is the configured issuer.
func (p *Provider) Issuer() string { return p.cfg.Issuer }

// Random returns a URL-safe random string (states, nonces, verifiers).
func Random() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func (p *Provider) getJSON(ctx context.Context, u string, header map[string]string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := p.cfg.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s: HTTP %d", u, resp.StatusCode)
	}
	return json.Unmarshal(body, out)
}

// endpointOK requires endpoints to be https unless the issuer itself
// is plain http (a provider on the home network).
func (p *Provider) endpointOK(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return false
	}
	return u.Scheme == "https" || (u.Scheme == "http" && strings.HasPrefix(p.cfg.Issuer, "http://"))
}

func (p *Provider) discover(ctx context.Context) (*discovery, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.meta != nil && p.cfg.Now().Sub(p.metaAt) < time.Hour {
		return p.meta, nil
	}
	var d discovery
	if err := p.getJSON(ctx, p.cfg.Issuer+"/.well-known/openid-configuration", nil, &d); err != nil {
		return nil, fmt.Errorf("OIDC discovery: %w", err)
	}
	if strings.TrimRight(d.Issuer, "/") != p.cfg.Issuer {
		return nil, fmt.Errorf("OIDC discovery: the provider calls itself %q, not %q (check the issuer URL)", d.Issuer, p.cfg.Issuer)
	}
	for _, e := range []string{d.AuthorizationEndpoint, d.TokenEndpoint, d.JWKSURI} {
		if !p.endpointOK(e) {
			return nil, fmt.Errorf("OIDC discovery: bad endpoint %q", e)
		}
	}
	if d.UserinfoEndpoint != "" && !p.endpointOK(d.UserinfoEndpoint) {
		d.UserinfoEndpoint = ""
	}
	p.meta, p.metaAt = &d, p.cfg.Now()
	return p.meta, nil
}

// Challenge is the PKCE S256 challenge for a verifier.
func Challenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// AuthURL is where to send the browser to sign in.
func (p *Provider) AuthURL(ctx context.Context, state, nonce, verifier string) (string, error) {
	d, err := p.discover(ctx)
	if err != nil {
		return "", err
	}
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {p.cfg.ClientID},
		"redirect_uri":          {p.cfg.RedirectURL},
		"scope":                 {strings.Join(p.cfg.Scopes, " ")},
		"state":                 {state},
		"nonce":                 {nonce},
		"code_challenge":        {Challenge(verifier)},
		"code_challenge_method": {"S256"},
	}
	sep := "?"
	if strings.Contains(d.AuthorizationEndpoint, "?") {
		sep = "&"
	}
	return d.AuthorizationEndpoint + sep + q.Encode(), nil
}

// Exchange trades an authorization code for the signed-in person's
// verified claims.
func (p *Provider) Exchange(ctx context.Context, code, verifier, nonce string) (*Claims, error) {
	d, err := p.discover(ctx)
	if err != nil {
		return nil, err
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {p.cfg.RedirectURL},
		"code_verifier": {verifier},
	}
	basic := true
	if len(d.TokenAuthMethods) > 0 && !contains(d.TokenAuthMethods, "client_secret_basic") && contains(d.TokenAuthMethods, "client_secret_post") {
		basic = false
		form.Set("client_id", p.cfg.ClientID)
		form.Set("client_secret", p.cfg.ClientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if basic {
		req.SetBasicAuth(url.QueryEscape(p.cfg.ClientID), url.QueryEscape(p.cfg.ClientSecret))
	}
	resp, err := p.cfg.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("OIDC token request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var tok struct {
		IDToken     string `json:"id_token"`
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
		ErrorDesc   string `json:"error_description"`
	}
	_ = json.Unmarshal(body, &tok)
	if resp.StatusCode/100 != 2 || tok.IDToken == "" {
		msg := tok.Error
		if tok.ErrorDesc != "" {
			msg += ": " + tok.ErrorDesc
		}
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d, no ID token", resp.StatusCode)
		}
		return nil, fmt.Errorf("OIDC token request refused (%s)", msg)
	}
	claims, err := p.Verify(ctx, tok.IDToken, nonce)
	if err != nil {
		return nil, err
	}
	// Some providers only put email or groups in userinfo.
	missing := claims.Email == ""
	for _, w := range p.cfg.Wanted {
		if _, ok := claims.Raw[w]; !ok {
			missing = true
		}
	}
	if d.UserinfoEndpoint != "" && tok.AccessToken != "" && missing {
		var info map[string]any
		if err := p.getJSON(ctx, d.UserinfoEndpoint, map[string]string{"Authorization": "Bearer " + tok.AccessToken}, &info); err == nil {
			if sub, _ := info["sub"].(string); sub == claims.Subject {
				for k, v := range info {
					if _, ok := claims.Raw[k]; !ok {
						claims.Raw[k] = v
					}
				}
				fillClaims(claims)
			}
		}
	}
	return claims, nil
}

// Verify checks an ID token's signature and claims.
func (p *Provider) Verify(ctx context.Context, token, nonce string) (*Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("ID token: not a JWT")
	}
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if err := decodeSegment(parts[0], &hdr); err != nil {
		return nil, fmt.Errorf("ID token header: %w", err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, errors.New("ID token: bad signature encoding")
	}
	key, err := p.key(ctx, hdr.Kid, hdr.Alg)
	if err != nil {
		return nil, err
	}
	if err := verifySig(hdr.Alg, key, []byte(parts[0]+"."+parts[1]), sig); err != nil {
		return nil, err
	}
	raw := map[string]any{}
	if err := decodeSegment(parts[1], &raw); err != nil {
		return nil, fmt.Errorf("ID token claims: %w", err)
	}
	c := &Claims{Raw: raw}
	fillClaims(c)
	now := p.cfg.Now()
	const skew = 2 * time.Minute
	iss, _ := raw["iss"].(string)
	d, _ := p.discover(ctx)
	if d == nil || iss != d.Issuer {
		return nil, fmt.Errorf("ID token from the wrong issuer (%q)", iss)
	}
	aud := c.Strings("aud")
	if !contains(aud, p.cfg.ClientID) {
		return nil, errors.New("ID token isn't for this client")
	}
	if azp, _ := raw["azp"].(string); len(aud) > 1 && azp != p.cfg.ClientID {
		return nil, errors.New("ID token's authorized party isn't this client")
	}
	exp, ok := numClaim(raw["exp"])
	if !ok || now.After(time.Unix(exp, 0).Add(skew)) {
		return nil, errors.New("ID token expired")
	}
	if iat, ok := numClaim(raw["iat"]); ok && time.Unix(iat, 0).After(now.Add(skew)) {
		return nil, errors.New("ID token issued in the future (check the clocks)")
	}
	if n, _ := raw["nonce"].(string); n == "" || n != nonce {
		return nil, errors.New("ID token nonce doesn't match (try signing in again)")
	}
	if c.Subject == "" {
		return nil, errors.New("ID token has no subject")
	}
	return c, nil
}

func fillClaims(c *Claims) {
	c.Subject, _ = c.Raw["sub"].(string)
	c.Email, _ = c.Raw["email"].(string)
	switch v := c.Raw["email_verified"].(type) {
	case bool:
		c.EmailVerified = v
	case string: // some providers send "true"
		c.EmailVerified = v == "true"
	}
	c.Name, _ = c.Raw["name"].(string)
	c.Username, _ = c.Raw["preferred_username"].(string)
}

func numClaim(v any) (int64, bool) {
	switch x := v.(type) {
	case float64:
		return int64(x), true
	case json.Number:
		n, err := x.Int64()
		return n, err == nil
	}
	return 0, false
}

func decodeSegment(s string, out any) error {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

// key finds the signing key, refetching the key set (at most once a
// minute) when the kid is new: providers rotate keys.
func (p *Provider) key(ctx context.Context, kid, alg string) (crypto.PublicKey, error) {
	want := "RSA"
	if strings.HasPrefix(alg, "ES") {
		want = "EC"
	}
	if !supportedAlg(alg) {
		return nil, fmt.Errorf("ID token signed with %q, which Thicket doesn't accept", alg)
	}
	find := func() crypto.PublicKey {
		if k, ok := p.keys[kid]; ok && kid != "" && k.kty == want {
			return k.key
		}
		if kid == "" { // only unambiguous without a kid
			var only crypto.PublicKey
			n := 0
			for _, k := range p.keys {
				if k.kty == want {
					only, n = k.key, n+1
				}
			}
			if n == 1 {
				return only
			}
		}
		return nil
	}
	p.mu.Lock()
	k := find()
	stale := p.keys == nil || p.cfg.Now().Sub(p.keysAt) > time.Minute
	p.mu.Unlock()
	if k != nil {
		return k, nil
	}
	if !stale {
		return nil, errors.New("ID token signed with an unknown key")
	}
	d, err := p.discover(ctx)
	if err != nil {
		return nil, err
	}
	var set struct {
		Keys []map[string]any `json:"keys"`
	}
	if err := p.getJSON(ctx, d.JWKSURI, nil, &set); err != nil {
		return nil, fmt.Errorf("OIDC keys: %w", err)
	}
	keys := map[string]jwk{}
	for i, raw := range set.Keys {
		k, err := parseJWK(raw)
		if err != nil {
			continue
		}
		if k.kid == "" {
			k.kid = fmt.Sprintf("#%d", i)
		}
		keys[k.kid] = k
	}
	p.mu.Lock()
	p.keys, p.keysAt = keys, p.cfg.Now()
	k = find()
	p.mu.Unlock()
	if k == nil {
		return nil, errors.New("ID token signed with an unknown key")
	}
	return k, nil
}

func parseJWK(m map[string]any) (jwk, error) {
	str := func(k string) string { s, _ := m[k].(string); return s }
	if use := str("use"); use != "" && use != "sig" {
		return jwk{}, errors.New("not a signing key")
	}
	b := func(k string) ([]byte, error) {
		return base64.RawURLEncoding.DecodeString(strings.TrimRight(str(k), "="))
	}
	k := jwk{kid: str("kid"), kty: str("kty")}
	switch k.kty {
	case "RSA":
		n, err1 := b("n")
		e, err2 := b("e")
		if err1 != nil || err2 != nil || len(n) < 256 || len(e) == 0 || len(e) > 4 {
			return jwk{}, errors.New("bad RSA key (at least 2048 bits)")
		}
		k.key = &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
	case "EC":
		var curve elliptic.Curve
		switch str("crv") {
		case "P-256":
			curve = elliptic.P256()
		case "P-384":
			curve = elliptic.P384()
		default:
			return jwk{}, errors.New("unsupported curve")
		}
		x, err1 := b("x")
		y, err2 := b("y")
		if err1 != nil || err2 != nil {
			return jwk{}, errors.New("bad EC key")
		}
		pub := &ecdsa.PublicKey{Curve: curve, X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
		if !curve.IsOnCurve(pub.X, pub.Y) {
			return jwk{}, errors.New("EC key not on its curve")
		}
		k.key = pub
	default:
		return jwk{}, errors.New("unsupported key type")
	}
	return k, nil
}

func supportedAlg(alg string) bool {
	switch alg {
	case "RS256", "RS384", "RS512", "PS256", "PS384", "PS512", "ES256", "ES384":
		return true
	}
	return false
}

func verifySig(alg string, key crypto.PublicKey, signed, sig []byte) error {
	var h crypto.Hash
	switch alg[2:] {
	case "256":
		h = crypto.SHA256
	case "384":
		h = crypto.SHA384
	case "512":
		h = crypto.SHA512
	}
	hh := h.New()
	hh.Write(signed)
	digest := hh.Sum(nil)
	bad := errors.New("ID token signature doesn't verify")
	switch {
	case strings.HasPrefix(alg, "RS"):
		pub, ok := key.(*rsa.PublicKey)
		if !ok || rsa.VerifyPKCS1v15(pub, h, digest, sig) != nil {
			return bad
		}
	case strings.HasPrefix(alg, "PS"):
		pub, ok := key.(*rsa.PublicKey)
		if !ok || rsa.VerifyPSS(pub, h, digest, sig, nil) != nil {
			return bad
		}
	case strings.HasPrefix(alg, "ES"):
		pub, ok := key.(*ecdsa.PublicKey)
		if !ok {
			return bad
		}
		size := (pub.Curve.Params().BitSize + 7) / 8
		if len(sig) != 2*size {
			return bad
		}
		r, s := new(big.Int).SetBytes(sig[:size]), new(big.Int).SetBytes(sig[size:])
		if !ecdsa.Verify(pub, digest, r, s) {
			return bad
		}
	default:
		return bad
	}
	return nil
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
