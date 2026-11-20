// Package matrix is the Matrix channel adapter, over the client-server
// API with /sync long polling (no open port needed). The bot is an
// ordinary Matrix account that posts under one name, so each reply starts
// with the sprig's name in bold.
//
// End-to-end encrypted rooms aren't supported: the bot refuses invites to
// them, and leaves a room (saying why) if encryption is turned on.
package matrix

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/theshield2594/thicket/internal/channel"
)

const (
	pollTimeout = 30 * time.Second // how long a /sync waits for events
	maxText     = 16000
	// maxSent is how many of the bot's own event ids are remembered, to
	// tell a reply to the bot from a reply to someone else.
	maxSent = 512
)

// EncryptedNotice is what the bot says (or gives as its reason) when it
// won't stay in an encrypted room.
const EncryptedNotice = "This room is end-to-end encrypted, and Thicket can't read encrypted rooms yet, so I'm leaving. " +
	"To talk to sprigs, start a chat with encryption turned off (in Element: create the room, then Settings, Security & Privacy, before anyone writes)."

type Adapter struct {
	// BaseURL is the homeserver's client-server API root, such as
	// https://matrix.example.org.
	BaseURL string
	token   string
	listen  map[string]bool
	log     *slog.Logger
	http    *http.Client
	poll    *http.Client

	connected   atomic.Bool
	userID      string
	displayName string

	txnBase string
	txn     atomic.Int64

	mu     sync.Mutex
	rooms  map[string]*room
	direct map[string]string // user id -> their DM room with the bot
	names  map[string]string // user id -> display name
	sent   map[string]bool   // the bot's own recent event ids
	sentQ  []string

	onReact func(ctx context.Context, r channel.Reaction)
}

// room is what the adapter knows about a joined room.
type room struct {
	members   int  // joined members, 0 if unknown
	direct    bool // a DM (invited with is_direct, or in m.direct)
	encrypted bool
	left      bool
}

// New makes an adapter for the account whose access token is token, on
// the homeserver at homeserver. listenRooms are room ids where sprigs
// answer every message.
func New(homeserver, token string, listenRooms []string, log *slog.Logger) *Adapter {
	l := map[string]bool{}
	for _, r := range listenRooms {
		l[r] = true
	}
	return &Adapter{BaseURL: strings.TrimRight(homeserver, "/"), token: token, listen: l, log: log,
		http:    &http.Client{Timeout: 30 * time.Second},
		poll:    &http.Client{Timeout: pollTimeout + 30*time.Second},
		txnBase: fmt.Sprintf("thk%d", time.Now().UnixNano()),
		rooms:   map[string]*room{}, direct: map[string]string{}, names: map[string]string{}, sent: map[string]bool{}}
}

func (a *Adapter) Name() string    { return "matrix" }
func (a *Adapter) Connected() bool { return a.connected.Load() }

// OnReaction registers what to call when someone reacts to a message.
// Call it before Run.
func (a *Adapter) OnReaction(f func(ctx context.Context, r channel.Reaction)) { a.onReact = f }

// --- client-server API ---

type apiError struct {
	Status     int
	Code       string // errcode, such as M_FORBIDDEN
	Msg        string
	RetryAfter time.Duration
}

func (e *apiError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("matrix: HTTP %d", e.Status)
	}
	return fmt.Sprintf("matrix: %s (%s, HTTP %d)", e.Msg, e.Code, e.Status)
}

// clean removes the access token from an error, should one ever contain it.
func (a *Adapter) clean(err error) error {
	if err == nil || a.token == "" || !strings.Contains(err.Error(), a.token) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), a.token, "[token]"))
}

// call sends a request to path (under /_matrix/client/v3 unless it starts
// with /_matrix) and reads the JSON answer into out.
func (a *Adapter) call(ctx context.Context, client *http.Client, method, path string, q url.Values, body, out any) error {
	if !strings.HasPrefix(path, "/_matrix/") {
		path = "/_matrix/client/v3" + path
	}
	u := a.BaseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return a.clean(err)
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return a.clean(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return a.clean(err)
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Errcode      string `json:"errcode"`
			Error        string `json:"error"`
			RetryAfterMS int64  `json:"retry_after_ms"`
		}
		_ = json.Unmarshal(raw, &e)
		return &apiError{Status: resp.StatusCode, Code: e.Errcode, Msg: e.Error, RetryAfter: time.Duration(e.RetryAfterMS) * time.Millisecond}
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("matrix %s: unreadable response (HTTP %d)", path, resp.StatusCode)
		}
	}
	return nil
}

// callRetry is call, retried once if the homeserver asks us to slow down.
func (a *Adapter) callRetry(ctx context.Context, method, path string, body, out any) error {
	err := a.call(ctx, a.http, method, path, nil, body, out)
	var ae *apiError
	if errors.As(err, &ae) && ae.Status == 429 && ae.RetryAfter <= 30*time.Second {
		wait := max(ae.RetryAfter, 500*time.Millisecond)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		err = a.call(ctx, a.http, method, path, nil, body, out)
	}
	return err
}

func (a *Adapter) nextTxn() string {
	return fmt.Sprintf("%s.%d", a.txnBase, a.txn.Add(1))
}

func esc(s string) string { return url.PathEscape(s) }

// --- events ---

type event struct {
	Type     string          `json:"type"`
	Sender   string          `json:"sender"`
	EventID  string          `json:"event_id"`
	StateKey *string         `json:"state_key"`
	Content  json.RawMessage `json:"content"`
}

type events struct {
	Events []event `json:"events"`
}

type joinedRoom struct {
	Summary struct {
		Joined *int `json:"m.joined_member_count"`
	} `json:"summary"`
	State    events `json:"state"`
	Timeline events `json:"timeline"`
}

type syncResponse struct {
	NextBatch   string `json:"next_batch"`
	AccountData events `json:"account_data"`
	Rooms       struct {
		Join   map[string]joinedRoom `json:"join"`
		Invite map[string]struct {
			State events `json:"invite_state"`
		} `json:"invite"`
		Leave map[string]json.RawMessage `json:"leave"`
	} `json:"rooms"`
}

type relatesTo struct {
	RelType   string `json:"rel_type"`
	EventID   string `json:"event_id"`
	Key       string `json:"key"`
	InReplyTo *struct {
		EventID string `json:"event_id"`
	} `json:"m.in_reply_to"`
}

type messageContent struct {
	MsgType       string `json:"msgtype"`
	Body          string `json:"body"`
	FormattedBody string `json:"formatted_body"`
	Filename      string `json:"filename"`
	URL           string `json:"url"`
	Info          struct {
		Mimetype string `json:"mimetype"`
		Size     int    `json:"size"`
	} `json:"info"`
	Mentions *struct {
		UserIDs []string `json:"user_ids"`
	} `json:"m.mentions"`
	RelatesTo *relatesTo `json:"m.relates_to"`
}

type memberContent struct {
	Membership  string `json:"membership"`
	DisplayName string `json:"displayname"`
	IsDirect    bool   `json:"is_direct"`
}

// --- receiving ---

// Bot is the bot's Matrix account and a link that opens a chat with it.
func (a *Adapter) Bot() (channel.Bot, bool) {
	if !a.connected.Load() {
		return channel.Bot{}, false
	}
	return channel.Bot{Name: a.userID, URL: "https://matrix.to/#/" + a.userID}, true
}

// whoami returns the account the access token belongs to.
func (a *Adapter) whoami(ctx context.Context) (string, error) {
	var me struct {
		UserID string `json:"user_id"`
	}
	err := a.call(ctx, a.http, http.MethodGet, "/account/whoami", nil, nil, &me)
	var ae *apiError
	if errors.As(err, &ae) && (ae.Status == 401 || ae.Code == "M_UNKNOWN_TOKEN" || ae.Code == "M_MISSING_TOKEN") {
		return "", errors.New("the Matrix homeserver didn't accept that access token")
	}
	if err != nil {
		return "", err
	}
	if me.UserID == "" {
		return "", errors.New("the Matrix homeserver gave an answer without a user id (is THICKET_MATRIX_HOMESERVER the client-server API address?)")
	}
	return me.UserID, nil
}

// Check asks the homeserver whose access token this is.
func (a *Adapter) Check(ctx context.Context) error {
	_, err := a.whoami(ctx)
	return err
}

// filter is the inline /sync filter: no presence, members loaded lazily,
// and limit timeline events per room.
func filter(limit int) string {
	return fmt.Sprintf(`{"presence":{"types":[]},"room":{"timeline":{"limit":%d},"state":{"lazy_load_members":true}}}`, limit)
}

func (a *Adapter) sync(ctx context.Context, since string, timeout time.Duration, limit int) (*syncResponse, error) {
	q := url.Values{"filter": {filter(limit)}, "timeout": {fmt.Sprint(timeout.Milliseconds())}}
	if since != "" {
		q.Set("since", since)
	}
	var r syncResponse
	if err := a.call(ctx, a.poll, http.MethodGet, "/sync", q, nil, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func (a *Adapter) Run(ctx context.Context, h channel.Handler) error {
	uid, err := a.whoami(ctx)
	if err != nil {
		return err
	}
	a.userID = uid
	var dn struct {
		DisplayName string `json:"displayname"`
	}
	if err := a.call(ctx, a.http, http.MethodGet, "/profile/"+esc(uid)+"/displayname", nil, nil, &dn); err == nil {
		a.displayName = dn.DisplayName
	}

	// The first sync only learns the rooms: messages sent while Thicket
	// was offline aren't answered hours later.
	var since string
	backoff := time.Second
	for since == "" {
		r, err := a.sync(ctx, "", 0, 1)
		if err == nil {
			a.handleSync(ctx, h, r, true)
			since = r.NextBatch
			break
		}
		if ctx.Err() != nil {
			return nil
		}
		var ae *apiError
		if errors.As(err, &ae) && ae.Status == 401 {
			return fmt.Errorf("matrix rejected the access token: %w", err)
		}
		a.log.Warn("matrix sync failed", "err", err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
	a.log.Info("matrix connected", "as", uid)
	a.connected.Store(true)
	defer a.connected.Store(false)

	backoff = time.Second
	for ctx.Err() == nil {
		r, err := a.sync(ctx, since, pollTimeout, 50)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			a.connected.Store(false)
			var ae *apiError
			if errors.As(err, &ae) && ae.Status == 401 {
				return fmt.Errorf("matrix rejected the access token: %w", err)
			}
			a.log.Warn("matrix sync failed", "err", err)
			select {
			case <-ctx.Done():
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		a.connected.Store(true)
		backoff = time.Second
		a.handleSync(ctx, h, r, false)
		if r.NextBatch != "" {
			since = r.NextBatch
		}
	}
	return nil
}

// handleSync applies a sync response. On the first sync (initial) only
// state is learnt; no message is delivered.
func (a *Adapter) handleSync(ctx context.Context, h channel.Handler, r *syncResponse, initial bool) {
	for _, ev := range r.AccountData.Events {
		if ev.Type == "m.direct" {
			a.learnDirect(ev.Content)
		}
	}
	for id, inv := range r.Rooms.Invite {
		a.invited(ctx, id, inv.State.Events)
	}
	for id := range r.Rooms.Leave {
		a.mu.Lock()
		if rm := a.rooms[id]; rm != nil {
			rm.left = true
		}
		a.mu.Unlock()
	}
	for id, jr := range r.Rooms.Join {
		rm := a.room(id)
		a.mu.Lock()
		if jr.Summary.Joined != nil {
			rm.members = *jr.Summary.Joined
		}
		rm.left = false
		a.mu.Unlock()
		for _, ev := range append(append([]event(nil), jr.State.Events...), jr.Timeline.Events...) {
			a.learnState(id, rm, ev)
		}
		a.mu.Lock()
		encrypted := rm.encrypted
		a.mu.Unlock()
		if encrypted {
			a.leaveEncrypted(ctx, id)
			continue
		}
		if initial {
			continue
		}
		for _, ev := range jr.Timeline.Events {
			switch ev.Type {
			case "m.room.message":
				if in, ok := a.incoming(id, rm, ev); ok {
					go h(ctx, in)
				}
			case "m.reaction":
				a.reaction(ctx, id, rm, ev)
			}
		}
	}
}

// room returns the room's record, creating it.
func (a *Adapter) room(id string) *room {
	a.mu.Lock()
	defer a.mu.Unlock()
	rm := a.rooms[id]
	if rm == nil {
		rm = &room{}
		a.rooms[id] = rm
	}
	return rm
}

// learnState notes what a state event says about a room.
func (a *Adapter) learnState(roomID string, rm *room, ev event) {
	if ev.StateKey == nil {
		return
	}
	switch ev.Type {
	case "m.room.encryption":
		a.mu.Lock()
		rm.encrypted = true
		a.mu.Unlock()
	case "m.room.member":
		var m memberContent
		if json.Unmarshal(ev.Content, &m) != nil {
			return
		}
		a.mu.Lock()
		if m.DisplayName != "" {
			a.names[*ev.StateKey] = m.DisplayName
		}
		if *ev.StateKey == a.userID && m.IsDirect && ev.Sender != a.userID {
			rm.direct = true
			if _, ok := a.direct[ev.Sender]; !ok {
				a.direct[ev.Sender] = roomID
			}
		}
		a.mu.Unlock()
	}
}

// learnDirect reads the bot's m.direct account data: its DM rooms, by user.
func (a *Adapter) learnDirect(raw json.RawMessage) {
	var m map[string][]string
	if json.Unmarshal(raw, &m) != nil {
		return
	}
	for user, rooms := range m {
		for _, id := range rooms {
			rm := a.room(id)
			a.mu.Lock()
			rm.direct = true
			a.direct[user] = id
			a.mu.Unlock()
		}
	}
}

// invited joins a room the bot was invited to, unless it is encrypted:
// that invite is declined with the reason, which clients show.
func (a *Adapter) invited(ctx context.Context, roomID string, state []event) {
	encrypted, direct, inviter := false, false, ""
	for _, ev := range state {
		switch {
		case ev.Type == "m.room.encryption":
			encrypted = true
		case ev.Type == "m.room.member" && ev.StateKey != nil && *ev.StateKey == a.userID:
			var m memberContent
			if json.Unmarshal(ev.Content, &m) == nil && m.Membership == "invite" {
				direct, inviter = m.IsDirect, ev.Sender
			}
		}
	}
	if encrypted {
		a.log.Warn("matrix: declined an invite to an encrypted room (not supported)", "room", roomID, "from", inviter)
		if err := a.callRetry(ctx, http.MethodPost, "/rooms/"+esc(roomID)+"/leave", map[string]any{"reason": EncryptedNotice}, nil); err != nil {
			a.log.Warn("matrix: couldn't decline an invite", "room", roomID, "err", err)
		}
		return
	}
	if err := a.callRetry(ctx, http.MethodPost, "/join/"+esc(roomID), map[string]any{}, nil); err != nil {
		a.log.Warn("matrix: couldn't join a room it was invited to", "room", roomID, "err", err)
		return
	}
	rm := a.room(roomID)
	a.mu.Lock()
	rm.left = false
	if direct && inviter != "" {
		rm.direct = true
		a.direct[inviter] = roomID
	}
	a.mu.Unlock()
	a.log.Info("matrix: joined a room", "room", roomID, "from", inviter, "dm", direct)
}

// leaveEncrypted says why, once, and leaves a room that turned on
// encryption. The notice itself goes unencrypted, which clients show.
func (a *Adapter) leaveEncrypted(ctx context.Context, roomID string) {
	a.mu.Lock()
	rm := a.rooms[roomID]
	if rm == nil || rm.left {
		a.mu.Unlock()
		return
	}
	rm.left = true
	a.mu.Unlock()
	a.log.Warn("matrix: leaving an encrypted room (not supported)", "room", roomID)
	_ = a.callRetry(ctx, http.MethodPut, "/rooms/"+esc(roomID)+"/send/m.room.message/"+esc(a.nextTxn()),
		map[string]any{"msgtype": "m.notice", "body": EncryptedNotice, "m.mentions": map[string]any{}}, nil)
	if err := a.callRetry(ctx, http.MethodPost, "/rooms/"+esc(roomID)+"/leave", map[string]any{"reason": "encrypted rooms aren't supported"}, nil); err != nil {
		a.log.Warn("matrix: couldn't leave an encrypted room", "room", roomID, "err", err)
	}
}

func (a *Adapter) isDM(rm *room) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return rm.direct || (rm.members > 0 && rm.members <= 2)
}

// incoming normalises a message event. ok is false for events to ignore.
func (a *Adapter) incoming(roomID string, rm *room, ev event) (channel.Incoming, bool) {
	// Never answer ourselves or other bots' notices: no bot-to-bot loops.
	if ev.Sender == "" || ev.Sender == a.userID {
		return channel.Incoming{}, false
	}
	var c messageContent
	if json.Unmarshal(ev.Content, &c) != nil {
		return channel.Incoming{}, false
	}
	if c.MsgType == "m.notice" || c.MsgType == "" {
		return channel.Incoming{}, false
	}
	replyTo := ""
	if c.RelatesTo != nil {
		if c.RelatesTo.RelType == "m.replace" { // an edit
			return channel.Incoming{}, false
		}
		if c.RelatesTo.InReplyTo != nil {
			replyTo = c.RelatesTo.InReplyTo.EventID
		}
	}
	text := c.Body
	var atts []channel.Attachment
	switch c.MsgType {
	case "m.image", "m.file", "m.audio", "m.video":
		name := c.Filename
		if name == "" {
			name, text = c.Body, ""
		} else if c.Body == c.Filename {
			text = "" // the body is just the file name, not a caption
		}
		if att, ok := a.attachment(c, name); ok {
			atts = append(atts, att)
		}
	case "m.text", "m.emote":
	default:
		if text == "" {
			return channel.Incoming{}, false
		}
	}
	if replyTo != "" {
		text = stripReplyFallback(text)
	}
	mentioned := false
	if c.Mentions != nil {
		for _, u := range c.Mentions.UserIDs {
			if u == a.userID {
				mentioned = true
			}
		}
	}
	if strings.Contains(c.FormattedBody, "matrix.to/#/"+a.userID) || strings.Contains(c.FormattedBody, "matrix.to/#/"+url.PathEscape(a.userID)) {
		mentioned = true
	}
	a.mu.Lock()
	if replyTo != "" && a.sent[replyTo] {
		mentioned = true
	}
	userName := a.names[ev.Sender]
	a.mu.Unlock()
	text, m2 := a.stripMentions(text)
	mentioned = mentioned || m2
	text = strings.TrimSpace(text)
	if text == "" && len(atts) == 0 {
		return channel.Incoming{}, false
	}
	if userName == "" {
		userName = localpart(ev.Sender)
	}
	return channel.Incoming{
		Channel: "matrix", ChatID: roomID, MessageID: ev.EventID,
		UserID: ev.Sender, UserName: userName, Text: text,
		IsDM: a.isDM(rm), Mentioned: mentioned, Listening: a.listen[roomID],
		Attachments: atts, ReplyTo: replyTo,
	}, true
}

// stripMentions removes the bot's user id, and its display name at the
// start of the text ("Thicket: hi", as clients write a mention), and
// reports whether either was there.
func (a *Adapter) stripMentions(text string) (string, bool) {
	mentioned := false
	if a.userID != "" && strings.Contains(text, a.userID) {
		mentioned = true
		text = strings.ReplaceAll(text, a.userID, "")
	}
	for _, n := range []string{a.displayName, localpart(a.userID)} {
		if n == "" || len(text) < len(n) || !strings.EqualFold(text[:len(n)], n) {
			continue
		}
		rest := text[len(n):]
		if r := strings.TrimLeft(rest, " "); strings.HasPrefix(r, ":") || strings.HasPrefix(r, ",") {
			mentioned = true
			text = r[1:]
			break
		}
	}
	return strings.TrimSpace(text), mentioned
}

// localpart is "alice" for "@alice:example.org".
func localpart(userID string) string {
	s := strings.TrimPrefix(userID, "@")
	if i := strings.IndexByte(s, ':'); i >= 0 {
		s = s[:i]
	}
	return s
}

// reaction hands an emoji reaction to the reaction handler.
func (a *Adapter) reaction(ctx context.Context, roomID string, rm *room, ev event) {
	if a.onReact == nil || ev.Sender == a.userID {
		return
	}
	var c struct {
		RelatesTo *relatesTo `json:"m.relates_to"`
	}
	if json.Unmarshal(ev.Content, &c) != nil || c.RelatesTo == nil || c.RelatesTo.RelType != "m.annotation" || c.RelatesTo.Key == "" {
		return
	}
	go a.onReact(ctx, channel.Reaction{Channel: "matrix", ChatID: roomID, MessageID: c.RelatesTo.EventID,
		UserID: ev.Sender, Emoji: strings.TrimRight(c.RelatesTo.Key, "️"), IsDM: a.isDM(rm)})
}

// attachment describes the file in a media message.
func (a *Adapter) attachment(c messageContent, name string) (channel.Attachment, bool) {
	server, media, ok := parseMXC(c.URL)
	if !ok {
		return channel.Attachment{}, false
	}
	ct := c.Info.Mimetype
	if ct == "" {
		switch c.MsgType {
		case "m.image":
			ct = "image/jpeg"
		case "m.audio":
			ct = "audio/ogg"
		case "m.video":
			ct = "video/mp4"
		}
	}
	return channel.Attachment{Name: name, ContentType: ct, Size: c.Info.Size,
		Open: func(ctx context.Context, max int) ([]byte, string, error) { return a.download(ctx, server, media, max) }}, true
}

// parseMXC splits mxc://server/media.
func parseMXC(u string) (server, media string, ok bool) {
	rest, found := strings.CutPrefix(u, "mxc://")
	if !found {
		return "", "", false
	}
	server, media, ok = strings.Cut(rest, "/")
	if !ok || server == "" || media == "" || strings.Contains(media, "/") {
		return "", "", false
	}
	return server, media, true
}

// download fetches a file from the homeserver: the authenticated media
// API, or the older unauthenticated one for homeservers without it.
func (a *Adapter) download(ctx context.Context, server, media string, max int) ([]byte, string, error) {
	paths := []string{
		"/_matrix/client/v1/media/download/" + esc(server) + "/" + esc(media),
		"/_matrix/media/v3/download/" + esc(server) + "/" + esc(media),
	}
	var lastErr error
	for i, p := range paths {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.BaseURL+p, nil)
		if err != nil {
			return nil, "", a.clean(err)
		}
		req.Header.Set("Authorization", "Bearer "+a.token)
		resp, err := a.http.Do(req)
		if err != nil {
			return nil, "", a.clean(err)
		}
		if i == 0 && (resp.StatusCode == 404 || resp.StatusCode == 400 || resp.StatusCode == 405) {
			resp.Body.Close()
			lastErr = fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, int64(max)+1))
		resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			return nil, "", fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
		}
		if err != nil {
			return nil, "", a.clean(err)
		}
		if len(body) > max {
			return nil, "", fmt.Errorf("too large (limit %s)", channel.HumanSize(max))
		}
		return body, resp.Header.Get("Content-Type"), nil
	}
	return nil, "", lastErr
}

// --- sending ---

func (a *Adapter) Send(ctx context.Context, chatID string, p channel.Persona, text string) error {
	_, err := a.SendIDs(ctx, chatID, p, text)
	return err
}

// SendIDs posts text as a persona, its name in bold first, and returns the
// event ids it became.
func (a *Adapter) SendIDs(ctx context.Context, chatID string, p channel.Persona, text string) ([]string, error) {
	var ids []string
	for i, part := range split(text, maxText) {
		body, html := part, toHTML(part)
		if i == 0 && p.Name != "" {
			body = p.Name + ":\n" + part
			html = "<strong>" + escape(p.Name) + "</strong><br>" + html
		}
		content := map[string]any{"msgtype": "m.text", "body": body,
			"format": "org.matrix.custom.html", "formatted_body": html,
			// Nobody is pinged by what a sprig happens to write.
			"m.mentions": map[string]any{}}
		var sent struct {
			EventID string `json:"event_id"`
		}
		if err := a.callRetry(ctx, http.MethodPut, "/rooms/"+esc(chatID)+"/send/m.room.message/"+esc(a.nextTxn()), content, &sent); err != nil {
			return ids, err
		}
		if sent.EventID != "" {
			ids = append(ids, sent.EventID)
			a.rememberSent(sent.EventID)
		}
	}
	return ids, nil
}

// rememberSent keeps the bot's event ids, to recognise replies to it.
func (a *Adapter) rememberSent(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sent[id] {
		return
	}
	a.sent[id] = true
	a.sentQ = append(a.sentQ, id)
	for len(a.sentQ) > maxSent {
		delete(a.sent, a.sentQ[0])
		a.sentQ = a.sentQ[1:]
	}
}

// SendDirect posts in the bot's DM room with a user, creating it (an
// invite they accept) if there is none.
func (a *Adapter) SendDirect(ctx context.Context, userID string, p channel.Persona, text string) (string, error) {
	a.mu.Lock()
	roomID := a.direct[userID]
	if rm := a.rooms[roomID]; roomID != "" && rm != nil && rm.left {
		roomID = ""
	}
	a.mu.Unlock()
	if roomID == "" {
		var created struct {
			RoomID string `json:"room_id"`
		}
		err := a.callRetry(ctx, http.MethodPost, "/createRoom", map[string]any{
			"is_direct": true, "invite": []string{userID}, "preset": "trusted_private_chat"}, &created)
		if err != nil {
			return "", fmt.Errorf("couldn't open a Matrix chat with %s: %w", userID, err)
		}
		roomID = created.RoomID
		rm := a.room(roomID)
		a.mu.Lock()
		rm.direct = true
		a.direct[userID] = roomID
		dm := map[string][]string{}
		for u, r := range a.direct {
			dm[u] = append(dm[u], r)
		}
		a.mu.Unlock()
		// So the DM is still known after a restart.
		if err := a.callRetry(ctx, http.MethodPut, "/user/"+esc(a.userID)+"/account_data/m.direct", dm, nil); err != nil {
			a.log.Warn("matrix: couldn't save the DM list", "err", err)
		}
	}
	return roomID, a.Send(ctx, roomID, p, text)
}

// Delete redacts a message.
func (a *Adapter) Delete(ctx context.Context, chatID, messageID string) error {
	return a.callRetry(ctx, http.MethodPut, "/rooms/"+esc(chatID)+"/redact/"+esc(messageID)+"/"+esc(a.nextTxn()), map[string]any{}, nil)
}

func (a *Adapter) Typing(ctx context.Context, chatID string) {
	if a.userID == "" {
		return
	}
	_ = a.call(ctx, a.http, http.MethodPut, "/rooms/"+esc(chatID)+"/typing/"+esc(a.userID), nil,
		map[string]any{"typing": true, "timeout": 20000}, nil)
}
