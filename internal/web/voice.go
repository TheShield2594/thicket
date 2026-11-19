package web

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/theshield2594/thicket/internal/channel"
	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/store"
)

// Spoken replies in the web app: the audio is kept with the message it
// reads (web_audio) and played from .../messages/{id}/audio.

// SpeechFormat asks for MP3, which every browser plays.
func (a *Adapter) SpeechFormat() string { return "mp3" }

// SendVoice attaches a spoken reply to the text message it goes with, or
// posts it as a message of its own when that message isn't known.
func (a *Adapter) SendVoice(ctx context.Context, chatID string, p channel.Persona, v channel.Voice) error {
	uid, _, ok := parseChat(chatID)
	if !ok {
		return errors.New("web: bad chat id")
	}
	ct := channel.VoiceContentType(v.Format)
	if id, err := strconv.ParseInt(v.With, 10, 64); err == nil && id > 0 {
		if m, err := a.DB.WebMessageByID(ctx, chatID, id); err == nil && m.Role == "assistant" {
			if err := a.DB.AddWebAudio(ctx, m.ID, ct, v.Audio); err != nil {
				return err
			}
			m.Audio = true
			a.publish(uid, Event{Type: "audio", Chat: chatID, Message: m, ID: m.ID})
			return nil
		}
	}
	m, err := a.DB.AddWebMessage(ctx, uid, store.WebMessage{ChatID: chatID, Role: "assistant", Character: p.ID, Content: v.Text})
	if err != nil {
		return err
	}
	if err := a.DB.AddWebAudio(ctx, m.ID, ct, v.Audio); err != nil {
		return err
	}
	m.Audio = true
	a.publish(uid, Event{Type: "message", Chat: chatID, Message: &m})
	return nil
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
	w.Header().Set("Cache-Control", "private, max-age=86400")
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
