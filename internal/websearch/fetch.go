// Package websearch reads web pages and searches the web for sprigs.
//
// Page addresses come from the model, which may have been steered by what
// it read, so fetching refuses anything that isn't a public internet
// address: loopback, private networks, link-local (cloud metadata),
// carrier-grade NAT and the like. The check runs when connecting, after
// DNS, so redirects and DNS tricks can't get around it.
package websearch

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	maxPageBytes = 2 << 20
	MaxPageChars = 12000
	fetchTimeout = 20 * time.Second
	maxRedirects = 5
)

// ErrBlocked means the address isn't on the public internet.
var ErrBlocked = errors.New("that address is on a private network or this machine, which sprigs can't fetch from")

var blockedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"), // benchmarking
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"), // NAT64 can reach IPv4 private space
}

// Public reports whether an address is a normal internet address.
func Public(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsMulticast() || a.IsUnspecified() || a.IsInterfaceLocalMulticast() {
		return false
	}
	for _, p := range blockedPrefixes {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// Fetcher reads pages. The zero value is safe; AllowPrivate is for tests.
type Fetcher struct {
	AllowPrivate bool
	client       *http.Client
}

func (f *Fetcher) httpClient() *http.Client {
	if f.client != nil {
		return f.client
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
		if f.AllowPrivate {
			return nil
		}
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip, err := netip.ParseAddr(host)
		if err != nil || !Public(ip) {
			return ErrBlocked
		}
		return nil
	}}
	f.client = &http.Client{
		Timeout: fetchTimeout,
		Transport: &http.Transport{
			Proxy:               nil, // a proxy would connect for us, past the check above
			DialContext:         dialer.DialContext,
			TLSHandshakeTimeout: 10 * time.Second,
			MaxIdleConns:        4,
			IdleConnTimeout:     30 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return errors.New("redirected to an unsupported address")
			}
			return nil
		},
	}
	return f.client
}

// Page is a fetched page as text.
type Page struct {
	URL       string // after redirects
	Title     string
	Text      string
	Truncated bool
}

// Fetch reads a page as plain text.
func (f *Fetcher) Fetch(ctx context.Context, raw string) (*Page, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("give a full http(s) address")
	}
	if u.User != nil {
		return nil, errors.New("addresses with a username or password aren't fetched")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Thicket/1 (personal assistant; reads a page for its user)")
	req.Header.Set("Accept", "text/html,text/plain,application/json,application/xml;q=0.9,*/*;q=0.1")
	resp, err := f.httpClient().Do(req)
	if err != nil {
		if errors.Is(err, ErrBlocked) {
			return nil, ErrBlocked
		}
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("couldn't load the page: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("the site answered HTTP %d", resp.StatusCode)
	}
	ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	textual := ct == "" || strings.HasPrefix(ct, "text/") || strings.Contains(ct, "json") || strings.Contains(ct, "xml")
	if !textual {
		return nil, fmt.Errorf("that's a %s file, not a page I can read", ct)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPageBytes))
	if err != nil {
		return nil, fmt.Errorf("couldn't read the page: %v", err)
	}
	if !utf8.Valid(body) {
		body = []byte(strings.ToValidUTF8(string(body), "�"))
	}
	p := &Page{URL: resp.Request.URL.String()}
	if ct == "text/html" || ct == "application/xhtml+xml" || (ct == "" && looksHTML(body)) {
		p.Title, p.Text = HTMLToText(string(body))
	} else {
		p.Text = strings.TrimSpace(string(body))
	}
	if len(p.Text) > MaxPageChars {
		cut := MaxPageChars
		for cut > 0 && !utf8.RuneStart(p.Text[cut]) {
			cut--
		}
		p.Text, p.Truncated = p.Text[:cut], true
	}
	return p, nil
}

func looksHTML(b []byte) bool {
	head := strings.ToLower(string(b[:min(len(b), 512)]))
	return strings.Contains(head, "<html") || strings.Contains(head, "<!doctype html")
}

var (
	commentRe    = regexp.MustCompile(`(?s)<!--.*?-->`)
	titleRe      = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	liOpenRe     = regexp.MustCompile(`(?i)<li\b[^>]*>`)
	liCloseRe    = regexp.MustCompile(`(?i)</li\s*>`)
	blockTagRe   = regexp.MustCompile(`(?i)</?(p|div|br|ul|ol|h[1-6]|tr|table|section|article|header|blockquote|pre|hr|dd|dt)\b[^>]*>`)
	cellTagRe    = regexp.MustCompile(`(?i)</?(td|th)\b[^>]*>`)
	tagRe        = regexp.MustCompile(`(?s)<[^>]*>`)
	spacesRe     = regexp.MustCompile(`[ \t\f\r\x{00a0}]+`)
	blankLinesRe = regexp.MustCompile(`\n\s*\n+`)
)

// dropBlocks are elements whose content is never text worth reading.
var dropBlocks = func() []*regexp.Regexp {
	var out []*regexp.Regexp
	for _, tag := range []string{"script", "style", "noscript", "svg", "template", "iframe", "head", "nav", "footer", "form"} {
		out = append(out, regexp.MustCompile(`(?is)<`+tag+`\b.*?</\s*`+tag+`\s*>`))
	}
	return out
}()

// HTMLToText turns a page into readable text: scripts, styles and page
// chrome removed, block elements on their own lines, entities decoded.
func HTMLToText(s string) (title, text string) {
	if m := titleRe.FindStringSubmatch(s); m != nil {
		title = strings.TrimSpace(html.UnescapeString(tagRe.ReplaceAllString(m[1], "")))
	}
	s = commentRe.ReplaceAllString(s, "")
	for _, re := range dropBlocks {
		s = re.ReplaceAllString(s, "\n")
	}
	s = liCloseRe.ReplaceAllString(s, "")
	s = liOpenRe.ReplaceAllString(s, "\n- ")
	s = blockTagRe.ReplaceAllString(s, "\n")
	s = cellTagRe.ReplaceAllString(s, " ")
	s = tagRe.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = spacesRe.ReplaceAllString(s, " ")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	s = blankLinesRe.ReplaceAllString(strings.Join(lines, "\n"), "\n\n")
	return title, strings.TrimSpace(s)
}
