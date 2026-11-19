// Package telegram is the Telegram channel adapter, over the Bot API with
// long polling (no public URL needed). A bot posts under one name, so
// each reply starts with the sprig's name in bold.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/theshield2594/thicket/internal/channel"
)

const (
	pollTimeout = 50 // seconds a getUpdates call waits for messages
	maxText     = 3500
	// An album arrives as one message per photo, sharing a media group id,
	// usually within a second. Parts are gathered until none has arrived
	// for albumWait, but never for longer than albumMaxWait.
	albumWait    = 1500 * time.Millisecond
	albumMaxWait = 5 * time.Second
)

type Adapter struct {
	token  string
	listen map[string]bool
	log    *slog.Logger
	// BaseURL is the Bot API root; tests point it at a fake.
	BaseURL string
	http    *http.Client
	poll    *http.Client

	connected   atomic.Bool
	botID       int64
	botUsername string

	albumsMu sync.Mutex
	albums   map[string]*album
	// albumWait and albumMaxWait are the constants above; tests shorten them.
	albumWait, albumMaxWait time.Duration

	onReact func(ctx context.Context, r channel.Reaction)
}

// OnReaction registers what to call when someone adds a reaction. Call it
// before Run.
func (a *Adapter) OnReaction(f func(ctx context.Context, r channel.Reaction)) { a.onReact = f }

// reactions turns a reaction update into the emoji newly added.
func (a *Adapter) reactions(r *reactionUpdate) []channel.Reaction {
	if r.User == nil || r.User.IsBot || r.Chat.Type == "channel" {
		return nil
	}
	had := map[string]bool{}
	for _, o := range r.Old {
		had[o.Emoji] = true
	}
	var out []channel.Reaction
	for _, n := range r.New {
		if n.Type != "emoji" || n.Emoji == "" || had[n.Emoji] {
			continue
		}
		out = append(out, channel.Reaction{Channel: "telegram", ChatID: strconv.FormatInt(r.Chat.ID, 10), MessageID: strconv.FormatInt(r.MessageID, 10),
			UserID: strconv.FormatInt(r.User.ID, 10), Emoji: n.Emoji, IsDM: r.Chat.Type == "private"})
	}
	return out
}

// album gathers the messages of one media group.
type album struct {
	first time.Time
	timer *time.Timer
	parts []albumPart
}

type albumPart struct {
	id int64
	in channel.Incoming
}

func New(token string, listenChats []string, log *slog.Logger) *Adapter {
	l := map[string]bool{}
	for _, c := range listenChats {
		l[c] = true
	}
	return &Adapter{token: token, listen: l, log: log, BaseURL: "https://api.telegram.org",
		http:      &http.Client{Timeout: 30 * time.Second},
		poll:      &http.Client{Timeout: (pollTimeout + 20) * time.Second},
		albums:    map[string]*album{},
		albumWait: albumWait, albumMaxWait: albumMaxWait}
}

func (a *Adapter) Name() string    { return "telegram" }
func (a *Adapter) Connected() bool { return a.connected.Load() }

// --- Bot API ---

type apiError struct {
	Code       int
	Desc       string
	RetryAfter int
}

func (e *apiError) Error() string { return fmt.Sprintf("telegram API error %d: %s", e.Code, e.Desc) }

// clean removes the bot token from anything that might be shown or
// logged: URLs with the token end up inside net/http errors.
func (a *Adapter) clean(err error) error {
	if err == nil || a.token == "" || !strings.Contains(err.Error(), a.token) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), a.token, "[token]"))
}

func (a *Adapter) call(ctx context.Context, client *http.Client, method string, params, out any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.BaseURL+"/bot"+a.token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return a.clean(err)
	}
	req.Header.Set("Content-Type", "application/json")
	return a.do(client, req, method, out)
}

// upload is call for methods that send a file: multipart, with the file
// as field (named name) and params as plain fields.
func (a *Adapter) upload(ctx context.Context, method string, params map[string]string, field, name, contentType string, data []byte, out any) error {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for k, v := range params {
		if err := mw.WriteField(k, v); err != nil {
			return err
		}
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, field, name))
	h.Set("Content-Type", contentType)
	part, err := mw.CreatePart(h)
	if err != nil {
		return err
	}
	if _, err := part.Write(data); err != nil {
		return err
	}
	if err := mw.Close(); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.BaseURL+"/bot"+a.token+"/"+method, &body)
	if err != nil {
		return a.clean(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return a.do(a.http, req, method, out)
}

// do sends a Bot API request and reads its result into out.
func (a *Adapter) do(client *http.Client, req *http.Request, method string, out any) error {
	resp, err := client.Do(req)
	if err != nil {
		return a.clean(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return a.clean(err)
	}
	var r struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		ErrorCode   int             `json:"error_code"`
		Description string          `json:"description"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return fmt.Errorf("telegram %s: unreadable response (HTTP %d)", method, resp.StatusCode)
	}
	if !r.OK {
		code := r.ErrorCode
		if code == 0 {
			code = resp.StatusCode
		}
		return &apiError{Code: code, Desc: r.Description, RetryAfter: r.Parameters.RetryAfter}
	}
	if out != nil {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}

// callRetry retries once when Telegram asks us to slow down.
func (a *Adapter) callRetry(ctx context.Context, method string, params, out any) error {
	return retry(ctx, func() error { return a.call(ctx, a.http, method, params, out) })
}

// retry runs f, and once more if Telegram asks us to slow down.
func retry(ctx context.Context, f func() error) error {
	err := f()
	var ae *apiError
	if errors.As(err, &ae) && ae.Code == 429 && ae.RetryAfter > 0 && ae.RetryAfter <= 30 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(ae.RetryAfter) * time.Second):
		}
		err = f()
	}
	return err
}

type user struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
}

type entity struct {
	Type   string `json:"type"`
	Offset int    `json:"offset"`
	Length int    `json:"length"`
	User   *user  `json:"user"`
}

type file struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
	MimeType string `json:"mime_type"`
	FileSize int    `json:"file_size"`
}

type message struct {
	MessageID int64 `json:"message_id"`
	From      *user `json:"from"`
	Chat      struct {
		ID   int64  `json:"id"`
		Type string `json:"type"` // private | group | supergroup | channel
	} `json:"chat"`
	Text            string   `json:"text"`
	Caption         string   `json:"caption"`
	Entities        []entity `json:"entities"`
	CaptionEntities []entity `json:"caption_entities"`
	ReplyTo         *struct {
		MessageID int64 `json:"message_id"`
		From      *user `json:"from"`
	} `json:"reply_to_message"`
	Photo    []file `json:"photo"`
	Document *file  `json:"document"`
	Voice    *file  `json:"voice"`
	Audio    *file  `json:"audio"`
	Video    *file  `json:"video"`
	// MediaGroupID is shared by the messages of one album.
	MediaGroupID string `json:"media_group_id"`
}

type update struct {
	UpdateID int64    `json:"update_id"`
	Message  *message `json:"message"`
	// Reaction is someone changing their reactions to a message.
	Reaction *reactionUpdate `json:"message_reaction"`
}

type reactionType struct {
	Type  string `json:"type"` // emoji | custom_emoji | paid
	Emoji string `json:"emoji"`
}

type reactionUpdate struct {
	Chat struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	} `json:"chat"`
	MessageID int64          `json:"message_id"`
	User      *user          `json:"user"`
	Old       []reactionType `json:"old_reaction"`
	New       []reactionType `json:"new_reaction"`
}

// --- receiving ---

// Bot is the bot account and the link that opens a chat with it.
func (a *Adapter) Bot() (channel.Bot, bool) {
	if !a.connected.Load() { // the username is set before this turns true
		return channel.Bot{}, false
	}
	return channel.Bot{Name: "@" + a.botUsername, URL: "https://t.me/" + a.botUsername}, true
}

// Check asks Telegram whether the token is a bot's.
func (a *Adapter) Check(ctx context.Context) error {
	var me user
	err := a.call(ctx, a.http, "getMe", struct{}{}, &me)
	var ae *apiError
	if errors.As(err, &ae) && (ae.Code == 401 || ae.Code == 404) {
		return errors.New("Telegram didn't accept that bot token (copy it again from @BotFather)")
	}
	return err
}

func (a *Adapter) Run(ctx context.Context, h channel.Handler) error {
	var me user
	if err := a.call(ctx, a.http, "getMe", struct{}{}, &me); err != nil {
		return fmt.Errorf("%w (check the bot token)", err)
	}
	a.botID, a.botUsername = me.ID, me.Username
	// Updates can't be polled while a webhook is set for this bot.
	if err := a.call(ctx, a.http, "deleteWebhook", map[string]any{"drop_pending_updates": false}, nil); err != nil {
		a.log.Warn("telegram deleteWebhook failed", "err", err)
	}
	a.log.Info("telegram connected", "as", me.Username)
	a.connected.Store(true)
	defer a.connected.Store(false)

	var offset int64
	backoff := time.Second
	for ctx.Err() == nil {
		var ups []update
		err := a.call(ctx, a.poll, "getUpdates", map[string]any{"offset": offset, "timeout": pollTimeout, "allowed_updates": a.allowedUpdates()}, &ups)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			a.connected.Store(false)
			var ae *apiError
			switch {
			case errors.As(err, &ae) && ae.Code == 401:
				return fmt.Errorf("telegram rejected the bot token: %w", err)
			case errors.As(err, &ae) && ae.Code == 409:
				a.log.Error("telegram: another program is polling this bot (is a second Thicket running?)", "err", err)
			default:
				a.log.Warn("telegram poll failed", "err", err)
			}
			select {
			case <-ctx.Done():
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		a.connected.Store(true)
		backoff = time.Second
		for _, u := range ups {
			offset = max(offset, u.UpdateID+1)
			if u.Reaction != nil && a.onReact != nil {
				for _, r := range a.reactions(u.Reaction) {
					go a.onReact(ctx, r)
				}
			}
			if u.Message == nil {
				continue
			}
			if in, ok := a.incoming(u.Message); ok {
				a.deliver(ctx, h, u.Message, in)
			}
		}
	}
	return nil
}

// allowedUpdates are the update kinds polled: messages, and reactions
// when someone listens for them (Telegram sends reactions only if asked).
func (a *Adapter) allowedUpdates() []string {
	if a.onReact != nil {
		return []string{"message", "message_reaction"}
	}
	return []string{"message"}
}

// deliver hands a message to the engine, holding album parts back until
// the whole album has arrived so it gets one reply, not one per photo.
func (a *Adapter) deliver(ctx context.Context, h channel.Handler, m *message, in channel.Incoming) {
	if m.MediaGroupID == "" {
		go h(ctx, in)
		return
	}
	key := in.ChatID + "/" + m.MediaGroupID
	a.albumsMu.Lock()
	defer a.albumsMu.Unlock()
	al := a.albums[key]
	if al == nil {
		al = &album{first: time.Now()}
		al.timer = time.AfterFunc(a.albumWait, func() { a.flushAlbum(ctx, h, key, al) })
		a.albums[key] = al
	} else if wait := min(a.albumWait, a.albumMaxWait-time.Since(al.first)); wait > 0 && al.timer.Stop() {
		// Still gathering: wait a little longer for the rest. If Stop
		// fails, the flush is already waiting for the lock and will
		// include this part.
		al.timer.Reset(wait)
	}
	al.parts = append(al.parts, albumPart{id: m.MessageID, in: in})
}

// flushAlbum merges an album's parts into one message: the caption as the
// text (Telegram usually puts it on one photo only) and every attachment,
// in order.
func (a *Adapter) flushAlbum(ctx context.Context, h channel.Handler, key string, al *album) {
	a.albumsMu.Lock()
	if a.albums[key] == al {
		delete(a.albums, key)
	}
	parts := al.parts
	al.parts = nil
	a.albumsMu.Unlock()
	if len(parts) == 0 || ctx.Err() != nil {
		return
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].id < parts[j].id })
	in := parts[0].in
	in.Attachments = nil
	var texts []string
	for _, p := range parts {
		if p.in.Text != "" {
			texts = append(texts, p.in.Text)
		}
		in.Mentioned = in.Mentioned || p.in.Mentioned
		in.Attachments = append(in.Attachments, p.in.Attachments...)
	}
	in.Text = strings.Join(texts, "\n\n")
	h(ctx, in)
}

// incoming normalises a message. ok is false for messages to ignore.
func (a *Adapter) incoming(m *message) (channel.Incoming, bool) {
	// Never react to bots (including ourselves): no bot-to-bot loops.
	if m.From == nil || m.From.IsBot || m.Chat.Type == "channel" {
		return channel.Incoming{}, false
	}
	text, ents := m.Text, m.Entities
	if text == "" {
		text, ents = m.Caption, m.CaptionEntities
	}
	mentioned := m.ReplyTo != nil && m.ReplyTo.From != nil && m.ReplyTo.From.ID == a.botID
	text, mentioned = a.stripMentions(text, ents, mentioned)
	// /start opens every new chat with a bot: treat it as a hello, unless
	// it came from an invite link (t.me/bot?start=join-...): then it's the
	// invite code.
	if cmd, arg, _ := strings.Cut(strings.TrimSpace(text), " "); strings.EqualFold(cmd, "/start") {
		text = "Hello!"
		if arg = strings.TrimSpace(arg); len(arg) > 5 && strings.EqualFold(arg[:5], "join-") {
			text = arg
		}
		mentioned = true
	}
	atts := a.attachments(m)
	text = strings.TrimSpace(text)
	if text == "" && len(atts) == 0 {
		return channel.Incoming{}, false
	}
	name := strings.TrimSpace(m.From.FirstName + " " + m.From.LastName)
	if name == "" {
		name = m.From.Username
	}
	chatID := strconv.FormatInt(m.Chat.ID, 10)
	replyTo := ""
	if m.ReplyTo != nil && m.ReplyTo.MessageID != 0 {
		replyTo = strconv.FormatInt(m.ReplyTo.MessageID, 10)
	}
	return channel.Incoming{
		Channel: "telegram", ChatID: chatID, MessageID: strconv.FormatInt(m.MessageID, 10),
		UserID: strconv.FormatInt(m.From.ID, 10), UserName: name, Text: text,
		IsDM: m.Chat.Type == "private", Mentioned: mentioned, Listening: a.listen[chatID],
		Attachments: atts, ReplyTo: replyTo,
	}, true
}

// stripMentions removes @-mentions of this bot (and /cmd@bot suffixes)
// from text, reporting whether the bot was mentioned. Offsets are in
// UTF-16 code units, as Telegram counts them.
func (a *Adapter) stripMentions(text string, ents []entity, mentioned bool) (string, bool) {
	u16 := utf16Units(text)
	var cut [][2]int
	for _, e := range ents {
		if e.Offset < 0 || e.Length <= 0 || e.Offset+e.Length > len(u16) {
			continue
		}
		part := fromUTF16(u16[e.Offset : e.Offset+e.Length])
		switch {
		case e.Type == "mention" && a.botUsername != "" && strings.EqualFold(part, "@"+a.botUsername):
			mentioned = true
			cut = append(cut, [2]int{e.Offset, e.Offset + e.Length})
		case e.Type == "text_mention" && e.User != nil && e.User.ID == a.botID:
			mentioned = true
		case e.Type == "bot_command" && a.botUsername != "" && strings.HasSuffix(strings.ToLower(part), "@"+strings.ToLower(a.botUsername)):
			mentioned = true
			n := len(utf16Units("@" + a.botUsername))
			cut = append(cut, [2]int{e.Offset + e.Length - n, e.Offset + e.Length})
		}
	}
	for i := len(cut) - 1; i >= 0; i-- { // entities come in order
		u16 = append(u16[:cut[i][0]], u16[cut[i][1]:]...)
	}
	return strings.TrimSpace(fromUTF16(u16)), mentioned
}

func (a *Adapter) attachments(m *message) []channel.Attachment {
	var out []channel.Attachment
	add := func(f *file, name, ct string) {
		if f.FileName != "" {
			name = f.FileName
		}
		if f.MimeType != "" {
			ct = f.MimeType
		}
		id := f.FileID
		out = append(out, channel.Attachment{Name: name, ContentType: ct, Size: f.FileSize,
			Open: func(ctx context.Context, max int) ([]byte, string, error) { return a.download(ctx, id, max) }})
	}
	if n := len(m.Photo); n > 0 {
		add(&m.Photo[n-1], "photo.jpg", "image/jpeg") // sizes come smallest first
	}
	if m.Document != nil {
		add(m.Document, "document", "")
	}
	if m.Voice != nil {
		add(m.Voice, "voice message.ogg", "audio/ogg")
	}
	if m.Audio != nil {
		add(m.Audio, "audio", "")
	}
	if m.Video != nil {
		add(m.Video, "video.mp4", "video/mp4")
	}
	return out
}

// download fetches a file through getFile. The file URL contains the
// token, so every error is cleaned.
func (a *Adapter) download(ctx context.Context, fileID string, max int) ([]byte, string, error) {
	var f struct {
		FilePath string `json:"file_path"`
	}
	if err := a.callRetry(ctx, "getFile", map[string]any{"file_id": fileID}, &f); err != nil {
		var ae *apiError
		if errors.As(err, &ae) && strings.Contains(strings.ToLower(ae.Desc), "too big") {
			return nil, "", fmt.Errorf("too large for a Telegram bot to download (limit 20 MB)")
		}
		return nil, "", a.clean(err)
	}
	if f.FilePath == "" {
		return nil, "", errors.New("Telegram didn't give a download path")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.BaseURL+"/file/bot"+a.token+"/"+f.FilePath, nil)
	if err != nil {
		return nil, "", a.clean(err)
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, "", a.clean(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, "", fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(max)+1))
	if err != nil {
		return nil, "", a.clean(err)
	}
	if len(body) > max {
		return nil, "", fmt.Errorf("too large (limit %s)", channel.HumanSize(max))
	}
	return body, resp.Header.Get("Content-Type"), nil
}

// --- sending ---

func (a *Adapter) Send(ctx context.Context, chatID string, p channel.Persona, text string) error {
	_, err := a.SendIDs(ctx, chatID, p, text)
	return err
}

// SendIDs posts text as a persona and returns the ids of the messages it
// became.
func (a *Adapter) SendIDs(ctx context.Context, chatID string, p channel.Persona, text string) ([]string, error) {
	var ids []string
	for i, part := range split(text, maxText) {
		var sent struct {
			MessageID int64 `json:"message_id"`
		}
		html := toHTML(part)
		if i == 0 && p.Name != "" {
			html = "<b>" + escape(p.Name) + "</b>\n" + html
		}
		params := map[string]any{"chat_id": chatID, "text": html, "parse_mode": "HTML",
			"link_preview_options": map[string]any{"is_disabled": true}}
		err := a.callRetry(ctx, "sendMessage", params, &sent)
		var ae *apiError
		if errors.As(err, &ae) && ae.Code == 400 && strings.Contains(ae.Desc, "parse entities") {
			// Formatting Telegram won't accept: send it plain.
			plain := part
			if i == 0 && p.Name != "" {
				plain = p.Name + ":\n" + part
			}
			err = a.callRetry(ctx, "sendMessage", map[string]any{"chat_id": chatID, "text": plain}, &sent)
		}
		if err != nil {
			return ids, err
		}
		if sent.MessageID != 0 {
			ids = append(ids, strconv.FormatInt(sent.MessageID, 10))
		}
	}
	return ids, nil
}

// SendDirect posts in the user's private chat, whose id is the user's id.
// Telegram only allows it once the user has started the bot.
func (a *Adapter) SendDirect(ctx context.Context, userID string, p channel.Persona, text string) (string, error) {
	err := a.Send(ctx, userID, p, text)
	var ae *apiError
	if errors.As(err, &ae) && ae.Code == 403 {
		err = fmt.Errorf("Telegram won't let me message them until they open a chat with the bot and press Start: %w", err)
	}
	return userID, err
}

// SpeechFormat asks for Ogg Opus: what Telegram voice notes are.
func (a *Adapter) SpeechFormat() string { return "opus" }

// SendVoice posts a spoken reply. Ogg Opus goes out as a voice note
// (sendVoice); anything else (a speech server that ignored the format)
// as an audio file (sendAudio), which Telegram plays inline too.
func (a *Adapter) SendVoice(ctx context.Context, chatID string, p channel.Persona, v channel.Voice) error {
	format := v.Format
	if format == "" {
		format = channel.VoiceFormat(v.Audio, "")
	}
	params := map[string]string{"chat_id": chatID}
	if p.Name != "" {
		params["caption"] = "<b>" + escape(p.Name) + "</b>"
		params["parse_mode"] = "HTML"
	}
	method, field, name := "sendVoice", "voice", "voice.ogg"
	if format != "ogg" {
		ext := format
		if ext == "" {
			ext = "mp3"
		}
		method, field, name = "sendAudio", "audio", "voice."+ext
		if p.Name != "" {
			params["title"] = p.Name
		}
	}
	err := retry(ctx, func() error {
		return a.upload(ctx, method, params, field, name, channel.VoiceContentType(format), v.Audio, nil)
	})
	var ae *apiError
	if method == "sendVoice" && errors.As(err, &ae) && ae.Code == 400 && strings.Contains(strings.ToUpper(ae.Desc), "VOICE_MESSAGES_FORBIDDEN") {
		// The person turned off receiving voice messages: send a file.
		delete(params, "parse_mode")
		params["caption"] = p.Name
		err = retry(ctx, func() error {
			return a.upload(ctx, "sendAudio", params, "audio", "voice.ogg", "audio/ogg", v.Audio, nil)
		})
	}
	return err
}

func (a *Adapter) Delete(ctx context.Context, chatID, messageID string) error {
	return a.callRetry(ctx, "deleteMessage", map[string]any{"chat_id": chatID, "message_id": messageID}, nil)
}

func (a *Adapter) Typing(ctx context.Context, chatID string) {
	_ = a.call(ctx, a.http, "sendChatAction", map[string]any{"chat_id": chatID, "action": "typing"}, nil)
}
