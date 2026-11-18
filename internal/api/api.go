// Package api is Thicket's local HTTP API: scripts can message a sprig and
// get its reply, list sprigs and jobs, and run a job now.
//
// The API is an internal chat channel ("api"), so a message takes the same
// path as one from Discord or Telegram (secret interception, approvals,
// rate limits, handoffs), and the replies are returned in the response.
// Each token acts as one user.
package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/theshield2594/thicket/internal/agent"
	"github.com/theshield2594/thicket/internal/channel"
	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/scheduler"
	"github.com/theshield2594/thicket/internal/store"
)

const (
	TokenPrefix    = "thk_api_"
	maxMessage     = 8000
	maxPayload     = 64 << 10
	messageTimeout = 3 * time.Minute
)

// NewToken returns a new API token and the hash to store.
func NewToken() (token, hash string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	token = TokenPrefix + base64.RawURLEncoding.EncodeToString(b)
	return token, HashToken(token), nil
}

func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Reply is one message a sprig sent in answer.
type Reply struct {
	SprigID string `json:"sprig_id"`
	Sprig   string `json:"sprig"`
	Text    string `json:"text"`
}

type API struct {
	Engine *agent.Engine
	DB     *store.Store
	Sched  *scheduler.Scheduler
	Chars  *character.Registry
	Log    *slog.Logger

	mu       sync.Mutex
	collect  map[string]*[]Reply    // chat id -> replies of the request in flight
	chatLock map[string]*sync.Mutex // one request at a time per conversation
	// A stop also sets aside requests already waiting for the
	// conversation: each records the stop count when it starts waiting,
	// and gives up if it changed by the time its turn comes.
	stops   map[string]int
	waiting map[string]int
	seq     atomic.Int64
}

func New(e *agent.Engine, db *store.Store, sched *scheduler.Scheduler, chars *character.Registry, log *slog.Logger) *API {
	return &API{Engine: e, DB: db, Sched: sched, Chars: chars, Log: log,
		collect: map[string]*[]Reply{}, chatLock: map[string]*sync.Mutex{}, stops: map[string]int{}, waiting: map[string]int{}}
}

// --- channel.Channel: replies are collected for the waiting request ---

func (a *API) Name() string    { return "api" }
func (a *API) Connected() bool { return true }

func (a *API) Run(ctx context.Context, _ channel.Handler) error {
	<-ctx.Done()
	return nil
}

func (a *API) Send(_ context.Context, chatID string, p channel.Persona, text string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if c := a.collect[chatID]; c != nil {
		*c = append(*c, Reply{SprigID: p.ID, Sprig: p.Name, Text: text})
		return nil
	}
	// Nobody is waiting (the request timed out): nowhere to deliver it.
	a.Log.Warn("api reply dropped: no request waiting", "chat", chatID)
	return errors.New("no API request is waiting for this reply")
}

func (a *API) SendDirect(context.Context, string, channel.Persona, string) (string, error) {
	return "", errors.New("the local API can't start conversations")
}

func (a *API) Delete(context.Context, string, string) error { return nil }
func (a *API) Typing(context.Context, string)               {}

// --- HTTP ---

type ctxKey struct{}

type caller struct {
	token *store.APIToken
	user  *store.User
}

// Handler serves /api/v1/.
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/messages", a.auth(a.postMessage))
	mux.HandleFunc("GET /api/v1/sprigs", a.auth(a.getSprigs))
	mux.HandleFunc("GET /api/v1/jobs", a.auth(a.getJobs))
	mux.HandleFunc("POST /api/v1/jobs/{id}/run", a.auth(a.runJob))
	mux.HandleFunc("POST /api/v1/stop", a.auth(a.stop))
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func (a *API) auth(next func(http.ResponseWriter, *http.Request, caller)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || !strings.HasPrefix(tok, TokenPrefix) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			fail(w, http.StatusUnauthorized, "missing or malformed token: send Authorization: Bearer thk_api_…")
			return
		}
		t, err := a.DB.APITokenByHash(r.Context(), HashToken(strings.TrimSpace(tok)))
		if err != nil {
			fail(w, http.StatusUnauthorized, "unknown or revoked token")
			return
		}
		u, err := a.DB.UserByID(r.Context(), t.UserID)
		if err != nil {
			fail(w, http.StatusUnauthorized, "the token's user no longer exists")
			return
		}
		next(w, r, caller{t, u})
	}
}

var convRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,39}$`)

func (a *API) postMessage(w http.ResponseWriter, r *http.Request, c caller) {
	var req struct {
		Text         string `json:"text"`
		Sprig        string `json:"sprig"`
		Conversation string `json:"conversation"`
		// NoApprovals stops this message from answering an approval
		// prompt (the MCP server always sets it).
		NoApprovals bool `json:"no_approvals"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxMessage*4)).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, "body must be JSON: {\"text\": \"…\"}")
		return
	}
	text := strings.TrimSpace(req.Text)
	if text == "" || len(text) > maxMessage {
		fail(w, http.StatusBadRequest, fmt.Sprintf("text is required (at most %d characters)", maxMessage))
		return
	}
	conv := strings.ToLower(req.Conversation)
	if conv == "" {
		conv = "default"
	}
	if !convRe.MatchString(conv) {
		fail(w, http.StatusBadRequest, "conversation: up to 40 lowercase letters, digits, - or _")
		return
	}
	if req.Sprig != "" {
		ch, ok := a.Chars.FindByName(req.Sprig) // id, name or nickname
		if !ok || !ch.UsableBy(c.user.ID) {
			fail(w, http.StatusNotFound, fmt.Sprintf("no sprig %q", req.Sprig))
			return
		}
		text = ch.ID + ", " + text // addressed by name, as in chat
	}

	// Each token has its own conversations, kept apart from chat apps'.
	chatID := fmt.Sprintf("t%d.%s", c.token.ID, conv)
	lock := a.lockFor(chatID)
	a.mu.Lock()
	stopsBefore := a.stops[chatID]
	a.waiting[chatID]++
	a.mu.Unlock()
	lock.Lock()
	defer lock.Unlock()
	a.mu.Lock()
	a.waiting[chatID]--
	setAside := a.stops[chatID] != stopsBefore
	a.mu.Unlock()
	if setAside {
		fail(w, http.StatusConflict, "set aside: a stop for this conversation came in while this message was waiting")
		return
	}
	var replies []Reply
	a.mu.Lock()
	a.collect[chatID] = &replies
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.collect, chatID)
		a.mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(r.Context(), messageTimeout)
	defer cancel()
	err := a.Engine.Handle(ctx, channel.Incoming{
		Channel: "api", ChatID: chatID, MessageID: "api-" + strconv.FormatInt(a.seq.Add(1), 10),
		UserID: strconv.FormatInt(c.user.ID, 10), UserName: c.user.Name, Text: text, IsDM: true, UserRef: c.user.ID,
		NoApprovals: req.NoApprovals,
	})
	a.mu.Lock()
	out := append([]Reply{}, replies...)
	a.mu.Unlock()
	switch {
	case errors.Is(err, agent.ErrRateLimited):
		fail(w, http.StatusTooManyRequests, err.Error())
	case err != nil && len(out) == 0:
		fail(w, http.StatusInternalServerError, "the message couldn't be handled; Thicket's activity log has details")
	default:
		writeJSON(w, http.StatusOK, map[string]any{"conversation": conv, "replies": out})
	}
}

// Queued is how many requests are waiting for a conversation ("t<token
// id>.<name>"). For tests.
func (a *API) Queued(chatID string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.waiting[chatID]
}

func (a *API) lockFor(chatID string) *sync.Mutex {
	a.mu.Lock()
	defer a.mu.Unlock()
	l := a.chatLock[chatID]
	if l == nil {
		l = &sync.Mutex{}
		a.chatLock[chatID] = l
	}
	return l
}

func (a *API) getSprigs(w http.ResponseWriter, _ *http.Request, c caller) {
	type sprig struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Role string `json:"role"`
		Host bool   `json:"host,omitempty"`
	}
	var out []sprig
	for _, ch := range a.Chars.For(c.user.ID) {
		out = append(out, sprig{ch.ID, ch.DisplayName(), ch.Role, ch.Host})
	}
	writeJSON(w, http.StatusOK, map[string]any{"sprigs": out})
}

type jobView struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Schedule string `json:"schedule,omitempty"`
	Sprig    string `json:"sprig"`
	Enabled  bool   `json:"enabled"`
	LastRun  string `json:"last_run,omitempty"`
}

func (a *API) getJobs(w http.ResponseWriter, r *http.Request, c caller) {
	jobs, err := a.DB.UserJobs(r.Context(), c.user.ID)
	if err != nil {
		fail(w, http.StatusInternalServerError, "couldn't list jobs")
		return
	}
	out := []jobView{}
	for _, j := range jobs {
		v := jobView{ID: j.ID, Name: j.Name, Kind: j.Kind, Schedule: j.Schedule, Sprig: j.Character, Enabled: j.Enabled}
		if runs, _ := a.DB.JobRuns(r.Context(), j.ID, 1); len(runs) > 0 {
			v.LastRun = runs[0].Status
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": out})
}

func (a *API) runJob(w http.ResponseWriter, r *http.Request, c caller) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		fail(w, http.StatusNotFound, "no such job")
		return
	}
	j, err := a.DB.JobByID(r.Context(), id)
	if err != nil || (j.UserID != c.user.ID && !c.user.IsAdmin()) {
		fail(w, http.StatusNotFound, "no such job")
		return
	}
	var req struct {
		Payload string `json:"payload"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxPayload+1024)).Decode(&req); err != nil {
			fail(w, http.StatusBadRequest, "body must be JSON: {\"payload\": \"…\"} (or empty)")
			return
		}
	}
	j.Payload = req.Payload
	switch err := a.Sched.Fire(*j); {
	case err == nil:
		writeJSON(w, http.StatusAccepted, map[string]any{"job": j.ID, "status": "started"})
	case errors.Is(err, scheduler.ErrPaused):
		fail(w, http.StatusConflict, err.Error())
	case errors.Is(err, scheduler.ErrBusy), errors.Is(err, scheduler.ErrTooSoon), errors.Is(err, scheduler.ErrTooMany):
		fail(w, http.StatusTooManyRequests, err.Error())
	default:
		a.Log.Error("api run job", "job", j.ID, "err", err)
		fail(w, http.StatusInternalServerError, "couldn't start the job")
	}
}

// stop stops the reply in progress in one of the token's conversations
// (or, with none running, the user's running jobs). It doesn't wait for
// the conversation's lock: the reply it stops is holding it.
func (a *API) stop(w http.ResponseWriter, r *http.Request, c caller) {
	var req struct {
		Conversation string `json:"conversation"`
	}
	// An empty body means the default conversation; anything else must be
	// valid, so a mistake can't stop the wrong reply.
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		fail(w, http.StatusBadRequest, "body must be JSON: {\"conversation\": \"…\"} (or empty)")
		return
	}
	conv := strings.ToLower(req.Conversation)
	if conv == "" {
		conv = "default"
	}
	if !convRe.MatchString(conv) {
		fail(w, http.StatusBadRequest, "conversation: up to 40 lowercase letters, digits, - or _")
		return
	}
	chatID := fmt.Sprintf("t%d.%s", c.token.ID, conv)
	// The stop and the count it leaves for waiting requests are one step:
	// a request that takes the conversation as soon as the stopped reply
	// lets go still sees it. (Engine.Stop doesn't block or call back here.)
	a.mu.Lock()
	stopped, jobs := a.Engine.Stop("api:"+chatID, c.user)
	setAside := 0
	if stopped {
		a.stops[chatID]++
		setAside = a.waiting[chatID]
	}
	a.mu.Unlock()
	if jobs == nil {
		jobs = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"stopped_reply": stopped, "stopped_jobs": jobs, "set_aside": setAside})
}
