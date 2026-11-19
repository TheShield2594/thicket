package mcp

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/theshield2594/thicket/internal/websearch"
)

// OAuth signs Thicket in to MCP servers that use OAuth (the MCP
// authorization spec): it finds the server's authorization server from
// the protected-resource and authorization-server metadata, registers
// itself as a client when the server allows it, sends the person a sign-in
// link with PKCE, and on the callback stores the tokens encrypted. Calls
// then get a fresh access token, refreshed when it expires or the server
// says it's no longer valid.
//
// A server set up this way has the header value OAuthHeader; the access
// token never goes into the server's stored config.

// OAuthHeader is the Authorization value for a server signed in with OAuth.
const OAuthHeader = "Bearer {{oauth}}"

// SecretStore is where tokens are kept (secrets.Store).
type SecretStore interface {
	Get(ctx context.Context, name string) (string, error)
	Put(ctx context.Context, name, value string) error
	Delete(ctx context.Context, name string) error
	// Remember adds a value to the redactor, so access tokens never reach
	// logs or the model.
	Remember(v string)
}

// OAuthSecretName is where a server's tokens are stored.
func OAuthSecretName(server string) string { return "oauth_" + server }

const (
	oauthPendingTTL = 15 * time.Minute
	oauthMaxPending = 20
	refreshEarly    = time.Minute
)

// ErrNotSignedIn means a server needs sign-in that hasn't happened (or
// whose tokens could no longer be refreshed).
var ErrNotSignedIn = errors.New("not signed in: ask Juni to connect it again to get a new sign-in link")

type OAuth struct {
	Secrets SecretStore
	HTTP    *http.Client
	// RedirectURL is the callback address the person's browser is sent
	// back to (THICKET_PUBLIC_URL + /oauth/callback).
	RedirectURL string
	// Now is the clock (tests).
	Now func() time.Time

	mu      sync.Mutex
	pending map[string]*pendingAuth // by state
	refresh sync.Mutex              // one refresh at a time
}

// OAuthTokens is what's stored per server.
type OAuthTokens struct {
	Access       string    `json:"access_token"`
	Refresh      string    `json:"refresh_token,omitempty"`
	Expiry       time.Time `json:"expiry,omitempty"`
	ClientID     string    `json:"client_id"`
	ClientSecret string    `json:"client_secret,omitempty"`
	AuthMethod   string    `json:"auth_method,omitempty"` // client_secret_basic | client_secret_post | none
	TokenURL     string    `json:"token_url"`
	Resource     string    `json:"resource"`
	Scope        string    `json:"scope,omitempty"`
	// PublicOnly: the MCP server is on a public address, so its sign-in
	// server must be too.
	PublicOnly bool `json:"public_only,omitempty"`
}

type pendingAuth struct {
	server   string
	verifier string
	tokens   OAuthTokens // client and endpoints; no tokens yet
	created  time.Time
	done     func(ctx context.Context, err error)
}

func (o *OAuth) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o *OAuth) client() *http.Client {
	if o.HTTP != nil {
		return o.HTTP
	}
	return oauthClient
}

// publicOnlyKey marks a context whose requests may only connect to
// public addresses.
type publicOnlyKey struct{}

// oauthClient checks, when connecting, that a request made for a public
// MCP server goes to a public address: the URLs it's sent to (resource
// metadata, authorization server, registration and token endpoints) come
// from that server, and mustn't point Thicket at its own network.
var oauthClient = &http.Client{
	Timeout: 20 * time.Second,
	Transport: &http.Transport{
		// Through a proxy the dial check below sees only the proxy, so
		// the target is checked here first.
		Proxy: func(r *http.Request) (*url.URL, error) {
			p, err := http.ProxyFromEnvironment(r)
			if p != nil && err == nil && r.Context().Value(publicOnlyKey{}) != nil && !publicHost(r.Context(), r.URL.String()) {
				return nil, errors.New("the sign-in server is on a private address, though the MCP server isn't")
			}
			return p, err
		},
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := &net.Dialer{Timeout: 10 * time.Second}
			if ctx.Value(publicOnlyKey{}) != nil {
				d.Control = func(_, address string, _ syscall.RawConn) error {
					host, _, err := net.SplitHostPort(address)
					if err != nil {
						return err
					}
					if ip, err := netip.ParseAddr(host); err != nil || !websearch.Public(ip) {
						return errors.New("the sign-in server is on a private address, though the MCP server isn't")
					}
					return nil
				}
			}
			return d.DialContext(ctx, network, address)
		},
		TLSHandshakeTimeout: 10 * time.Second,
		IdleConnTimeout:     30 * time.Second,
	},
}

// publicHost reports whether every address rawURL's host resolves to is
// public.
func publicHost(ctx context.Context, rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return false
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", u.Hostname())
	if err != nil || len(addrs) == 0 {
		return false
	}
	for _, a := range addrs {
		if !websearch.Public(a.Unmap()) {
			return false
		}
	}
	return true
}

// Start discovers how to sign in to the MCP server at serverURL and
// returns the link the person opens. done runs once they finish (or the
// sign-in fails at the callback). clientID and clientSecret are for
// servers that don't allow automatic registration; usually both are "".
func (o *OAuth) Start(ctx context.Context, server, serverURL, clientID, clientSecret string, done func(ctx context.Context, err error)) (string, error) {
	if o.RedirectURL == "" {
		return "", errors.New("no callback address: set THICKET_PUBLIC_URL")
	}
	public := publicHost(ctx, serverURL)
	if public {
		ctx = context.WithValue(ctx, publicOnlyKey{}, true)
	}
	meta, resource, err := o.discover(ctx, serverURL)
	if err != nil {
		return "", err
	}
	tok := OAuthTokens{ClientID: clientID, ClientSecret: clientSecret, TokenURL: meta.TokenEndpoint, Resource: resource, AuthMethod: "none", PublicOnly: public}
	if clientSecret != "" {
		tok.AuthMethod = "client_secret_post"
	}
	if tok.ClientID == "" {
		if meta.RegistrationEndpoint == "" {
			return "", errors.New("this server doesn't let apps register themselves: create an OAuth app there with the redirect URL " + o.RedirectURL + " and give me its client id (and secret, if it has one)")
		}
		if tok.ClientID, tok.ClientSecret, tok.AuthMethod, err = o.register(ctx, meta.RegistrationEndpoint); err != nil {
			return "", fmt.Errorf("registering with the sign-in server: %w", err)
		}
	}
	if len(meta.Scopes) > 0 {
		tok.Scope = strings.Join(meta.Scopes, " ")
	}
	verifier, state := randomString(32), randomString(32)
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {tok.ClientID},
		"redirect_uri":          {o.RedirectURL},
		"state":                 {state},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
		"resource":              {resource},
	}
	if tok.Scope != "" {
		q.Set("scope", tok.Scope)
	}
	sep := "?"
	if strings.Contains(meta.AuthorizationEndpoint, "?") {
		sep = "&"
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.pending == nil {
		o.pending = map[string]*pendingAuth{}
	}
	o.expireLocked()
	if len(o.pending) >= oauthMaxPending {
		return "", errors.New("too many sign-ins in progress; try again in a few minutes")
	}
	o.pending[state] = &pendingAuth{server: server, verifier: verifier, tokens: tok, created: o.now(), done: done}
	return meta.AuthorizationEndpoint + sep + q.Encode(), nil
}

func (o *OAuth) expireLocked() {
	for k, p := range o.pending {
		if o.now().Sub(p.created) > oauthPendingTTL {
			delete(o.pending, k)
		}
	}
}

// Callback finishes a sign-in: the browser came back with a code (or an
// error) for state. It returns the server's name.
func (o *OAuth) Callback(ctx context.Context, state, code, errCode, errDesc string) (string, error) {
	o.mu.Lock()
	o.expireLocked()
	p := o.pending[state]
	delete(o.pending, state) // single use, whatever happens
	o.mu.Unlock()
	if p == nil || state == "" {
		return "", errors.New("this sign-in link has expired or was already used; ask Juni for a new one")
	}
	err := func() error {
		if errCode != "" {
			msg := errCode
			if errDesc != "" {
				msg += ": " + errDesc
			}
			return fmt.Errorf("the sign-in server said %s", truncateText(msg, 200))
		}
		if code == "" {
			return errors.New("the sign-in server sent no code")
		}
		form := url.Values{
			"grant_type":    {"authorization_code"},
			"code":          {code},
			"redirect_uri":  {o.RedirectURL},
			"code_verifier": {p.verifier},
			"resource":      {p.tokens.Resource},
		}
		t := p.tokens
		if err := o.token(ctx, &t, form); err != nil {
			return err
		}
		return o.save(ctx, p.server, t)
	}()
	if p.done != nil {
		// Connecting and telling the person can take a model call: the
		// browser shouldn't wait for it.
		go p.done(context.WithoutCancel(ctx), err)
	}
	return p.server, err
}

// AccessToken returns a current access token for server, refreshing it
// if it has expired (or force is set: the server just refused it).
func (o *OAuth) AccessToken(ctx context.Context, server string, force bool) (string, error) {
	o.refresh.Lock()
	defer o.refresh.Unlock()
	t, err := o.load(ctx, server)
	if err != nil {
		return "", err
	}
	stale := !t.Expiry.IsZero() && o.now().Add(refreshEarly).After(t.Expiry)
	if !force && !stale {
		return t.Access, nil
	}
	if t.Refresh == "" {
		if force || o.now().After(t.Expiry) {
			return "", ErrNotSignedIn
		}
		return t.Access, nil
	}
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {t.Refresh}, "resource": {t.Resource}}
	if err := o.token(ctx, &t, form); err != nil {
		var te *tokenError
		if errors.As(err, &te) && te.code == "invalid_grant" {
			return "", ErrNotSignedIn
		}
		return "", fmt.Errorf("refreshing the sign-in: %w", err)
	}
	if err := o.save(ctx, server, t); err != nil {
		return "", err
	}
	return t.Access, nil
}

// Forget deletes a server's tokens.
func (o *OAuth) Forget(ctx context.Context, server string) error {
	return o.Secrets.Delete(ctx, OAuthSecretName(server))
}

func (o *OAuth) load(ctx context.Context, server string) (OAuthTokens, error) {
	var t OAuthTokens
	raw, err := o.Secrets.Get(ctx, OAuthSecretName(server))
	if err != nil || raw == "" {
		return t, ErrNotSignedIn
	}
	if err := json.Unmarshal([]byte(raw), &t); err != nil || t.Access == "" {
		return t, ErrNotSignedIn
	}
	o.Secrets.Remember(t.Access)
	o.Secrets.Remember(t.Refresh)
	return t, nil
}

func (o *OAuth) save(ctx context.Context, server string, t OAuthTokens) error {
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	o.Secrets.Remember(t.Access)
	o.Secrets.Remember(t.Refresh)
	return o.Secrets.Put(ctx, OAuthSecretName(server), string(b))
}

type tokenError struct {
	status int
	code   string
	desc   string
}

func (e *tokenError) Error() string {
	if e.code != "" {
		return fmt.Sprintf("token endpoint: %s %s (HTTP %d)", e.code, truncateText(e.desc, 200), e.status)
	}
	return fmt.Sprintf("token endpoint: HTTP %d", e.status)
}

// token calls the token endpoint and updates t with what comes back.
func (o *OAuth) token(ctx context.Context, t *OAuthTokens, form url.Values) error {
	if t.PublicOnly {
		ctx = context.WithValue(ctx, publicOnlyKey{}, true)
	}
	if t.AuthMethod != "client_secret_basic" {
		form.Set("client_id", t.ClientID)
	}
	if t.AuthMethod == "client_secret_post" {
		form.Set("client_secret", t.ClientSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if t.AuthMethod == "client_secret_basic" {
		req.SetBasicAuth(url.QueryEscape(t.ClientID), url.QueryEscape(t.ClientSecret))
	}
	resp, err := o.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out struct {
		Access    string `json:"access_token"`
		Refresh   string `json:"refresh_token"`
		ExpiresIn int64  `json:"expires_in"`
		Type      string `json:"token_type"`
		Scope     string `json:"scope"`
		Error     string `json:"error"`
		Desc      string `json:"error_description"`
	}
	_ = json.Unmarshal(raw, &out)
	if resp.StatusCode/100 != 2 || out.Error != "" || out.Access == "" {
		return &tokenError{status: resp.StatusCode, code: out.Error, desc: out.Desc}
	}
	if out.Type != "" && !strings.EqualFold(out.Type, "bearer") {
		return fmt.Errorf("token endpoint gave a %q token; only bearer tokens are supported", out.Type)
	}
	t.Access = out.Access
	if out.Refresh != "" {
		t.Refresh = out.Refresh // otherwise the old one stays valid
	}
	t.Expiry = time.Time{}
	if out.ExpiresIn > 0 {
		t.Expiry = o.now().Add(time.Duration(out.ExpiresIn) * time.Second)
	}
	if out.Scope != "" {
		t.Scope = out.Scope
	}
	return nil
}

// register does dynamic client registration (RFC 7591).
func (o *OAuth) register(ctx context.Context, endpoint string) (id, secret, method string, err error) {
	body, _ := json.Marshal(map[string]any{
		"client_name":                "Thicket",
		"redirect_uris":              []string{o.RedirectURL},
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := o.client().Do(req)
	if err != nil {
		return "", "", "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
		Method       string `json:"token_endpoint_auth_method"`
		Error        string `json:"error"`
		Desc         string `json:"error_description"`
	}
	_ = json.Unmarshal(raw, &out)
	if resp.StatusCode/100 != 2 || out.ClientID == "" {
		if out.Error != "" {
			return "", "", "", fmt.Errorf("%s %s (HTTP %d)", out.Error, truncateText(out.Desc, 200), resp.StatusCode)
		}
		return "", "", "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	method = out.Method
	switch {
	case out.ClientSecret == "":
		method = "none"
	case method == "" || (method != "client_secret_basic" && method != "client_secret_post"):
		method = "client_secret_basic" // RFC 7591's default
	}
	return out.ClientID, out.ClientSecret, method, nil
}

type asMetadata struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	RegistrationEndpoint  string   `json:"registration_endpoint"`
	ChallengeMethods      []string `json:"code_challenge_methods_supported"`
	Scopes                []string `json:"-"` // from the protected resource
}

// discover finds the authorization server for an MCP server: the
// resource metadata it points to on a 401 (or at the well-known
// address), then that server's metadata. Servers from before resource
// metadata existed are their own authorization server.
func (o *OAuth) discover(ctx context.Context, serverURL string) (*asMetadata, string, error) {
	su, err := url.Parse(serverURL)
	if err != nil || (su.Scheme != "https" && su.Scheme != "http") || su.Host == "" {
		return nil, "", errors.New("OAuth needs the server's http(s) URL")
	}
	resource := su.Scheme + "://" + su.Host + strings.TrimRight(su.EscapedPath(), "/")
	if su.Path == "" || su.Path == "/" {
		resource = su.Scheme + "://" + su.Host
	}
	prmURL, err := o.probe(ctx, serverURL)
	if err != nil {
		return nil, "", err
	}
	candidates := []string{}
	if prmURL != "" {
		candidates = append(candidates, prmURL)
	}
	if p := strings.TrimRight(su.EscapedPath(), "/"); p != "" {
		candidates = append(candidates, su.Scheme+"://"+su.Host+"/.well-known/oauth-protected-resource"+p)
	}
	candidates = append(candidates, su.Scheme+"://"+su.Host+"/.well-known/oauth-protected-resource")
	issuer := su.Scheme + "://" + su.Host // the old default
	var scopes []string
	for _, c := range candidates {
		var prm struct {
			Resource string   `json:"resource"`
			Servers  []string `json:"authorization_servers"`
			Scopes   []string `json:"scopes_supported"`
		}
		if o.getJSON(ctx, c, &prm) != nil || len(prm.Servers) == 0 {
			continue
		}
		issuer = strings.TrimRight(prm.Servers[0], "/")
		if prm.Resource != "" {
			resource = prm.Resource
		}
		scopes = prm.Scopes
		break
	}
	meta, err := o.asMeta(ctx, issuer)
	if err != nil {
		return nil, "", err
	}
	meta.Scopes = scopes
	if len(meta.ChallengeMethods) > 0 && !contains(meta.ChallengeMethods, "S256") {
		return nil, "", errors.New("the sign-in server doesn't support PKCE (S256), which Thicket requires")
	}
	for _, e := range []string{meta.AuthorizationEndpoint, meta.TokenEndpoint} {
		u, err := url.Parse(e)
		if err != nil || u.Host == "" || (u.Scheme != "https" && !(u.Scheme == "http" && su.Scheme == "http")) {
			return nil, "", fmt.Errorf("the sign-in server's endpoint %q isn't a usable https URL", e)
		}
	}
	return meta, resource, nil
}

// probe calls the server without credentials. A 401 is expected; its
// WWW-Authenticate header may name the resource metadata.
func (o *OAuth) probe(ctx context.Context, serverURL string) (string, error) {
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"` + protocolVersion + `","capabilities":{},"clientInfo":{"name":"thicket","version":"1"}}}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, serverURL, strings.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("MCP-Protocol-Version", protocolVersion)
	resp, err := o.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("couldn't reach the server: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode/100 == 2 {
		return "", errors.New("this server answered without signing in, so it doesn't need OAuth: connect it without auth")
	}
	return wwwAuthParam(resp.Header.Values("WWW-Authenticate"), "resource_metadata"), nil
}

// asMeta fetches authorization server metadata (RFC 8414, or OpenID
// discovery), trying the addresses the MCP spec lists in order.
func (o *OAuth) asMeta(ctx context.Context, issuer string) (*asMetadata, error) {
	iu, err := url.Parse(issuer)
	if err != nil || iu.Host == "" {
		return nil, fmt.Errorf("bad authorization server %q", issuer)
	}
	origin, p := iu.Scheme+"://"+iu.Host, strings.TrimRight(iu.EscapedPath(), "/")
	urls := []string{origin + "/.well-known/oauth-authorization-server" + p, origin + "/.well-known/openid-configuration" + p}
	if p != "" {
		urls = append(urls, origin+p+"/.well-known/openid-configuration")
	}
	for _, u := range urls {
		var m asMetadata
		if o.getJSON(ctx, u, &m) == nil && m.AuthorizationEndpoint != "" && m.TokenEndpoint != "" {
			if m.Issuer != "" && strings.TrimRight(m.Issuer, "/") != strings.TrimRight(issuer, "/") {
				return nil, fmt.Errorf("the sign-in server's metadata names a different issuer (%s)", m.Issuer)
			}
			return &m, nil
		}
	}
	if p == "" {
		// Servers from the 2025-03-26 spec without metadata: the
		// default endpoints at the server's own origin.
		return &asMetadata{Issuer: origin, AuthorizationEndpoint: origin + "/authorize", TokenEndpoint: origin + "/token", RegistrationEndpoint: origin + "/register"}, nil
	}
	return nil, fmt.Errorf("couldn't find the sign-in server's metadata at %s", issuer)
}

func (o *OAuth) getJSON(ctx context.Context, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := o.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

// wwwAuthParam reads one parameter of a Bearer challenge.
func wwwAuthParam(headers []string, name string) string {
	for _, h := range headers {
		for _, part := range strings.Split(h, ",") {
			part = strings.TrimSpace(part)
			if i := strings.IndexByte(part, ' '); i >= 0 && !strings.Contains(part[:i], "=") {
				part = strings.TrimSpace(part[i+1:]) // "Bearer resource_metadata=..."
			}
			k, v, ok := strings.Cut(part, "=")
			if ok && strings.EqualFold(strings.TrimSpace(k), name) {
				return strings.Trim(strings.TrimSpace(v), `"`)
			}
		}
	}
	return ""
}

func randomString(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func truncateText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
