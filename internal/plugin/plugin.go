// Package plugin knows Thicket's optional abilities that run in their own
// containers. A plugin is an MCP server in a container of its own,
// started by a docker compose profile; Thicket's core bundles none. When
// its address answers, Thicket connects it as a built-in integration
// named after it (the way the sandbox computer is): no sprig can use it
// until its owner grants mcp:<name>/*, and that grant is in the trail.
//
// This package is only the list of known plugins and how each is found:
// it has no dependencies, so config, app, agent and doctor can all use
// it. See docs/plugins.md for how to write one.
package plugin

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Spec describes one known plugin.
type Spec struct {
	// Name is the integration's name, so its tools are granted as
	// mcp:<Name>/*. Lower case letters and digits.
	Name string
	// Profile is the docker compose profile that starts it ("" = it is
	// part of the default stack).
	Profile string
	// DefaultURL is tried when THICKET_<NAME>_URL isn't set: if it
	// answers, the plugin is connected; if not, it is simply absent. ""
	// means the plugin is off unless its URL is set.
	DefaultURL string
	// Exec: the plugin runs commands or programs, so a sprig also needs
	// exec turned on by its owner to be offered its tools.
	Exec bool
	// KidsAllowed: kid sprigs may be granted its tools (still only ones
	// that only read, one by one, by a parent). Most plugins say no.
	KidsAllowed bool
	// Check is an optional extra check for `thicket doctor`, run after
	// the plugin answered and accepted its token. It gets the plugin's
	// base URL and token. note is added to the line either way (advice,
	// such as missing DNS records); err fails the check.
	Check func(ctx context.Context, url, token string) (note string, err error)
}

var nameRE = regexp.MustCompile(`^[a-z][a-z0-9]{1,30}$`)

// env names a plugin's environment variable: THICKET_<NAME>_<what>.
func (s Spec) env(what string) string { return "THICKET_" + strings.ToUpper(s.Name) + "_" + what }

// URLEnv is the environment variable that sets the plugin's address.
func (s Spec) URLEnv() string { return s.env("URL") }

// TokenFileEnv is the environment variable that says where the token
// shared with the plugin lives.
func (s Spec) TokenFileEnv() string { return s.env("TOKEN_FILE") }

// Secret is the stored secret that holds the token shared with it.
func (s Spec) Secret() string { return s.Name + "_token" }

// Scope is the grant that gives a sprig all of its tools.
func (s Spec) Scope() string { return "mcp:" + s.Name + "/*" }

// Registry is the list of known plugins.
type Registry struct {
	specs []Spec
}

// New makes a registry; names must be valid and distinct.
func New(specs ...Spec) (*Registry, error) {
	seen := map[string]bool{}
	for _, s := range specs {
		if !nameRE.MatchString(s.Name) {
			return nil, fmt.Errorf("plugin name %q: lower case letters and digits, starting with a letter", s.Name)
		}
		if seen[s.Name] {
			return nil, fmt.Errorf("plugin %q is listed twice", s.Name)
		}
		seen[s.Name] = true
	}
	return &Registry{specs: append([]Spec(nil), specs...)}, nil
}

// MustNew is New for fixed lists.
func MustNew(specs ...Spec) *Registry {
	r, err := New(specs...)
	if err != nil {
		panic(err)
	}
	return r
}

// Sandbox is sprigs' own Linux computer (thicket-sandbox, docs/sandbox.md).
// It is in the default stack, connected when THICKET_SANDBOX_URL is set
// (docker-compose.yml sets it).
var Sandbox = Spec{Name: "sandbox", Exec: true}

// Mail gives sprigs their own email addresses (thicket-mail,
// docs/mail.md): JMAP, or IMAP and SMTP. Kid sprigs can't have it.
var Mail = Spec{Name: "mail", Profile: "mail", DefaultURL: "http://mail:7072", Check: mailCheck}

// Voice hears voice messages and reads replies aloud on this machine
// (thicket-voice, docs/voice.md): whisper.cpp and Piper behind
// OpenAI-compatible endpoints that Thicket itself uses. Its only tool
// lists its voices.
var Voice = Spec{Name: "voice", Profile: "voice", DefaultURL: "http://voice:7071", Check: voiceCheck}

// Browser gives sprigs their own web browser (thicket-browser,
// docs/browser.md): one Chromium per sprig behind an egress allowlist.
// Kid sprigs can't have it.
var Browser = Spec{Name: "browser", Profile: "browser", DefaultURL: "http://browser:7073", Check: browserCheck}

// Builtin is every plugin Thicket knows.
func Builtin() *Registry { return MustNew(Sandbox, Mail, Voice, Browser) }

// All lists the plugins, in order.
func (r *Registry) All() []Spec {
	if r == nil {
		return nil
	}
	return append([]Spec(nil), r.specs...)
}

// Get finds a plugin by name.
func (r *Registry) Get(name string) (Spec, bool) {
	if r == nil {
		return Spec{}, false
	}
	for _, s := range r.specs {
		if s.Name == name {
			return s, true
		}
	}
	return Spec{}, false
}

// ScopeServer returns the integration a scope names ("mcp:browser/*" →
// "browser"), or "" if it isn't an integration scope.
func ScopeServer(scope string) string {
	rest, ok := strings.CutPrefix(scope, "mcp:")
	if !ok {
		return ""
	}
	server, _, _ := strings.Cut(rest, "/")
	return server
}

// KidRefused returns an error naming the first scope that grants a
// plugin kid sprigs may not have, or nil.
func (r *Registry) KidRefused(scopes []string) error {
	for _, sc := range scopes {
		if p, ok := r.Get(ScopeServer(sc)); ok && !p.KidsAllowed {
			return fmt.Errorf("kid sprigs can't use the %s plugin", p.Name)
		}
	}
	return nil
}

// Endpoint is where a plugin is, as configured.
type Endpoint struct {
	Spec
	// URL is its base address ("" = off), with no trailing slash.
	URL string
	// Explicit: the URL was set, so a plugin that doesn't answer is an
	// error worth reporting. Otherwise it is DefaultURL, tried quietly.
	Explicit bool
	// TokenFile is where the token shared with it lives.
	TokenFile string
}

// Resolve says where a plugin is. lookup reads the environment
// (os.LookupEnv); dataDir holds the token when no file is named.
// THICKET_<NAME>_URL set to "" or "off" turns the plugin off.
func (s Spec) Resolve(lookup func(string) (string, bool), dataDir string) Endpoint {
	ep := Endpoint{Spec: s}
	if v, ok := lookup(s.URLEnv()); ok {
		v = strings.TrimSpace(v)
		if v != "" && !strings.EqualFold(v, "off") {
			ep.URL, ep.Explicit = strings.TrimRight(v, "/"), true
		}
	} else {
		ep.URL = strings.TrimRight(s.DefaultURL, "/")
	}
	ep.TokenFile = filepath.Join(dataDir, s.Name, "token")
	if v, ok := lookup(s.TokenFileEnv()); ok && strings.TrimSpace(v) != "" {
		ep.TokenFile = strings.TrimSpace(v)
	}
	return ep
}

// MCPURL is the plugin's MCP endpoint.
func (ep Endpoint) MCPURL() string { return ep.URL + "/mcp" }

// Answers reports whether anything answers HTTP at a plugin's base URL
// (its /health), within a few seconds.
func Answers(ctx context.Context, url string) bool {
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, url+"/health", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}
