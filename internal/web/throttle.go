package web

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// Sign-in throttling. Tokens are unguessable, so the limits on failed
// sign-ins only stop floods and password guessing. They're counted per
// client first, so one client can't lock everyone else out, with
// household-wide backstops above them for an attacker with many
// addresses.
const (
	// Failed sign-ins (and admitted attempts still running) per client,
	// and in all, per window.
	clientFailLimit = 10
	loginFailLimit  = 120
	loginFailWindow = time.Minute
	// A username is locked for usernameLockFor after usernameFailLimit
	// wrong passwords in a row from one client, or usernameGlobalLimit
	// from all clients together.
	usernameFailLimit   = 5
	usernameGlobalLimit = 50
	usernameLockFor     = 15 * time.Minute
	// maxTracked bounds each map: a flood of made-up names or addresses
	// resets it rather than growing it.
	maxTracked = 10000
)

// clientIP is the address a request came from. Behind a reverse proxy on
// the same machine or network (the peer is loopback or private), it's the
// last address the proxy added to X-Forwarded-For; a client can put
// anything earlier in that header, but not the entry its proxy appends.
// Elsewhere the header is ignored.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	peer = peer.Unmap()
	if peer.IsLoopback() || peer.IsPrivate() {
		if xff := r.Header.Values("X-Forwarded-For"); len(xff) > 0 {
			parts := strings.Split(xff[len(xff)-1], ",")
			if a, err := netip.ParseAddr(strings.TrimSpace(parts[len(parts)-1])); err == nil {
				return a.Unmap().String()
			}
		}
	}
	return peer.String()
}

// loginThrottle limits failed sign-in attempts per window.
type loginThrottle struct {
	mu      sync.Mutex
	start   time.Time
	fails   int            // failed redemptions in this window, in all
	pending int            // admitted attempts still being redeemed, in all
	byFails map[string]int // failures in this window, per client
	byPend  map[string]int // attempts still being redeemed, per client
}

// loginBegin admits a sign-in attempt to the database, reserving a slot
// so a concurrent burst can't all pass before failures are counted. Every
// admitted attempt must call loginEnd.
func (a *Adapter) loginBegin(r *http.Request, now time.Time) bool {
	l := &a.logins
	client := clientIP(r)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.byPend == nil {
		l.byPend = map[string]int{}
	}
	if now.Sub(l.start) >= loginFailWindow || l.byFails == nil {
		l.start, l.fails, l.byFails = now, 0, map[string]int{}
	}
	if l.fails+l.pending >= loginFailLimit || l.byFails[client]+l.byPend[client] >= clientFailLimit {
		return false
	}
	if len(l.byFails) >= maxTracked || len(l.byPend) >= maxTracked {
		return false // the backstop: too many clients at once
	}
	l.pending++
	l.byPend[client]++
	return true
}

// loginEnd releases an admitted attempt's slot, counting it if it failed.
func (a *Adapter) loginEnd(r *http.Request, failed bool) {
	l := &a.logins
	client := clientIP(r)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pending--
	if l.byPend[client]--; l.byPend[client] <= 0 {
		delete(l.byPend, client)
	}
	if failed {
		l.fails++
		l.byFails[client]++
	}
}

// userThrottle locks a username after wrong passwords: per client, so
// someone guessing can't lock its owner out from everywhere, and in all,
// so many addresses can't guess without limit.
type userThrottle struct {
	mu    sync.Mutex
	fails map[string]int // client + name, and "" + name for all clients
	until map[string]time.Time
}

func throttleKey(client, name string) string {
	return client + "\x00" + strings.ToLower(name)
}

func (u *userThrottle) locked(client, name string, now time.Time) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return now.Before(u.until[throttleKey(client, name)]) || now.Before(u.until[throttleKey("", name)])
}

func (u *userThrottle) record(client, name string, ok bool, now time.Time) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.fails == nil {
		u.fails, u.until = map[string]int{}, map[string]time.Time{}
	}
	mine, all := throttleKey(client, name), throttleKey("", name)
	if ok {
		delete(u.fails, mine)
		return
	}
	for key, limit := range map[string]int{mine: usernameFailLimit, all: usernameGlobalLimit} {
		u.fails[key]++
		if u.fails[key] >= limit {
			u.until[key] = now.Add(usernameLockFor)
			delete(u.fails, key)
		}
	}
	if len(u.fails) > maxTracked {
		u.fails = map[string]int{}
	}
	if len(u.until) > maxTracked {
		for k, t := range u.until {
			if !now.Before(t) {
				delete(u.until, k)
			}
		}
	}
}
