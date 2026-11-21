// Package browser is Thicket's side of the thicket-browser plugin
// (plugins/browser, docs/browser.md): the allowlist's entry format, the
// name of each sprig's profile key, and a client for the plugin's admin
// API (doctor check, sealed profiles for backups).
package browser

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// Plugin is the plugin's integration name (mcp:browser/*).
const Plugin = "browser"

var hostRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)

// AllowEntry checks and normalises a browser allowlist entry: a site's
// host name, which allows it and its subdomains ("shop.example" allows
// www.shop.example). A pasted address is cut down to its host
// ("https://www.shop.example/cart" → "www.shop.example"); "*." in front
// is dropped. Addresses (IPs), single-label names (localhost, nas) and
// names under local-only suffixes are refused: the browser can never
// reach a private address anyway.
func AllowEntry(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil || u.Hostname() == "" {
			return "", fmt.Errorf("%q isn't a web address", s)
		}
		s = u.Hostname()
	} else if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	if h, _, err := net.SplitHostPort(s); err == nil {
		s = h
	}
	s = strings.TrimPrefix(strings.TrimPrefix(strings.TrimSuffix(s, "."), "*."), ".")
	if s == "" {
		return "", errors.New("give a site, like shop.example")
	}
	for _, r := range s {
		if r > 127 {
			return "", fmt.Errorf("%q: give the name in its xn-- form (international names aren't converted yet)", s)
		}
	}
	if strings.ContainsAny(s, "*?") {
		return "", fmt.Errorf("%q: no wildcards; a site's name already allows its subdomains", s)
	}
	if net.ParseIP(strings.Trim(s, "[]")) != nil {
		return "", fmt.Errorf("%q is an address: give the site's name (the browser never reaches private addresses, and public sites have names)", s)
	}
	if !hostRE.MatchString(s) {
		return "", fmt.Errorf("%q isn't a site's name (like shop.example)", s)
	}
	for _, suf := range []string{".local", ".lan", ".home", ".internal", ".localhost", ".home.arpa", ".localdomain", ".intranet", ".corp"} {
		if strings.HasSuffix(s, suf) || s == strings.TrimPrefix(suf, ".") {
			return "", fmt.Errorf("%q is a local name: the browser only reaches public internet sites", s)
		}
	}
	return s, nil
}

// Allowed reports whether host is on the list: an entry allows itself
// and its subdomains. It is what the plugin's proxy checks too.
func Allowed(list []string, host string) bool {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	for _, e := range list {
		if host == e || strings.HasSuffix(host, "."+e) {
			return true
		}
	}
	return false
}

var secretUnsafe = regexp.MustCompile(`[^a-z0-9_]`)

// ProfileKeySecret is the stored secret that holds a sprig's profile key.
func ProfileKeySecret(sprig string) string {
	return "browser_" + secretUnsafe.ReplaceAllString(strings.ToLower(sprig), "_") + "_profile_key"
}

// Check is one line of the plugin's doctor report.
type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// Doctor is the plugin's doctor report.
type Doctor struct {
	Checks []Check `json:"checks"`
	Open   []struct {
		Sprig string `json:"sprig"`
	} `json:"open"`
	Max int `json:"max"`
}

// Profile is a sealed profile the plugin keeps.
type Profile struct {
	Sprig string `json:"sprig"`
	Size  int64  `json:"size"`
	Open  bool   `json:"open"`
}

// Client talks to the plugin's admin API.
type Client struct {
	URL   string // base, no trailing slash
	Token func(ctx context.Context) (string, error)
	HTTP  *http.Client
}

// ErrNotFound is the plugin's 404.
var ErrNotFound = errors.New("not found")

func (c *Client) raw(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.URL+path, body)
	if err != nil {
		return nil, err
	}
	tok, err := c.Token(ctx)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 == 2 {
		return resp, nil
	}
	defer resp.Body.Close()
	var e struct {
		Error string `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&e)
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrNotFound
	}
	if e.Error == "" {
		e.Error = resp.Status
	}
	return nil, fmt.Errorf("browser plugin: %s", e.Error)
}

func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	resp, err := c.raw(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

// Doctor runs the plugin's own check: Chromium starts and the proxy
// refuses what it must.
func (c *Client) Doctor(ctx context.Context) (*Doctor, error) {
	var d Doctor
	return &d, c.getJSON(ctx, "/v1/doctor", &d)
}

// Profiles lists the sealed profiles.
func (c *Client) Profiles(ctx context.Context) ([]Profile, error) {
	var out struct {
		Profiles []Profile `json:"profiles"`
	}
	return out.Profiles, c.getJSON(ctx, "/v1/profiles", &out)
}

// maxProfile bounds a profile read from or sent to the plugin.
const maxProfile = 512 << 20

// Profile returns a sprig's sealed profile as it is at rest.
func (c *Client) Profile(ctx context.Context, sprig string) ([]byte, error) {
	resp, err := c.raw(ctx, http.MethodGet, "/v1/profiles/"+url.PathEscape(sprig), nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxProfile+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxProfile {
		return nil, errors.New("profile too large")
	}
	return b, nil
}

// PutProfile replaces a sprig's sealed profile (a restored backup).
func (c *Client) PutProfile(ctx context.Context, sprig string, sealed []byte) error {
	resp, err := c.raw(ctx, http.MethodPut, "/v1/profiles/"+url.PathEscape(sprig), bytes.NewReader(sealed))
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// LooksSealed reports whether b is a sealed browser profile.
func LooksSealed(b []byte) bool { return len(b) >= 32 && string(b[:4]) == "TBP1" }
