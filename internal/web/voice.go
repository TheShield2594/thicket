package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/theshield2594/thicket/internal/channel"
	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/store"
)

// Spoken replies in the web app: the audio is kept with the message it
// reads (web_audio) and played from .../messages/{id}/audio.

// The engine finds spoken replies by this interface: keep it implemented.
var _ channel.VoiceSender = (*Adapter)(nil)

// SpeechFormat asks for MP3, which every browser plays.
func (a *Adapter) SpeechFormat() string { return "mp3" }

// SendVoice attaches a spoken reply to the text message it goes with, or
// posts it as a message of its own when no message was named. If the
// named message is gone (deleted while the speech was made), nothing is
// posted: the person removed that reply. Attached audio adds no message,
// so no ids are returned for it.
func (a *Adapter) SendVoice(ctx context.Context, chatID string, p channel.Persona, v channel.Voice) ([]string, error) {
	uid, _, ok := parseChat(chatID)
	if !ok {
		return nil, errors.New("web: bad chat id")
	}
	ct := channel.VoiceContentType(v.Format)
	if v.With != "" {
		id, err := strconv.ParseInt(v.With, 10, 64)
		if err != nil || id <= 0 {
			return nil, fmt.Errorf("web: bad message id %q", v.With)
		}
		m, err := a.DB.WebMessageByID(ctx, chatID, id)
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil // deleted meanwhile: don't bring it back
		}
		if err != nil {
			return nil, err
		}
		if m.Role != "assistant" {
			return nil, fmt.Errorf("web: message %d isn't a sprig's reply", id)
		}
		if err := a.DB.AddWebAudio(ctx, m.ID, ct, v.Audio); err != nil {
			return nil, err
		}
		m.Audio = true
		a.publish(uid, Event{Type: "audio", Chat: chatID, Message: m, ID: m.ID})
		return nil, nil
	}
	m, err := a.DB.AddWebMessage(ctx, uid, store.WebMessage{ChatID: chatID, Role: "assistant", Character: p.ID, Content: v.Text})
	if err != nil {
		return nil, err
	}
	if err := a.DB.AddWebAudio(ctx, m.ID, ct, v.Audio); err != nil {
		return nil, err
	}
	m.Audio = true
	a.publish(uid, Event{Type: "message", Chat: chatID, Message: &m})
	return []string{strconv.FormatInt(m.ID, 10)}, nil
}

// audio serves a message's spoken reply.
func (a *Adapter) audio(w http.ResponseWriter, r *http.Request, u *store.User) {
	chatID := r.PathValue("chat")
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if _, ok := a.chatFor(u, chatID); !ok || err != nil {
		writeErr(w, http.StatusNotFound, "no such message")
		return
	}
	data, ct, err := a.DB.WebAudio(r.Context(), chatID, id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "no audio for that message")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	w.Header().Set("Content-Type", ct)
	// Personal audio: the browser keeps no copy, so nobody can replay it
	// from the cache after logging out.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Write(data)
}

// speaks is when a sprig answers this person out loud: never for a kid
// sprig a parent hasn't allowed to.
func speaks(c *character.Character) string {
	if c.IsKid() && !c.Allows(character.AllowVoice) {
		return character.SpeakNever
	}
	return c.SpeaksWhen()
}

// setVoice sets how a sprig speaks to this person: brief, full or off.
func (a *Adapter) setVoice(w http.ResponseWriter, r *http.Request, u *store.User) {
	c, ok := a.Chars.Get(r.PathValue("id"))
	if !ok || !c.UsableBy(u) {
		writeErr(w, http.StatusNotFound, "no such sprig")
		return
	}
	var b struct {
		Mode string `json:"mode"`
	}
	if !decodeBody(w, r, &b) {
		return
	}
	switch b.Mode {
	case store.SpeechBrief, store.SpeechFull, store.SpeechOff:
	default:
		writeErr(w, http.StatusBadRequest, "mode must be brief, full or off")
		return
	}
	if err := a.DB.SetSpeechMode(r.Context(), u.ID, c.ID, b.Mode); err != nil {
		writeErr(w, http.StatusInternalServerError, "database error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": c.ID, "voice_mode": b.Mode, "speaks": speaks(c)})
}
