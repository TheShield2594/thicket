package telegram

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/theshield2594/thicket/internal/channel"
)

func TestSendVoice(t *testing.T) {
	a, f := newTest(t)
	ctx := context.Background()
	p := channel.Persona{Name: "Pip & co"}
	// Ogg Opus is a voice note.
	if err := a.SendVoice(ctx, "42", p, channel.Voice{Audio: []byte("OggS opus"), Format: "ogg"}); err != nil {
		t.Fatal(err)
	}
	// Anything else is an audio file.
	if err := a.SendVoice(ctx, "42", p, channel.Voice{Audio: []byte("ID3 mp3"), Format: "mp3"}); err != nil {
		t.Fatal(err)
	}
	// Someone who blocks voice messages gets the note as a file.
	f.noVoice = true
	if err := a.SendVoice(ctx, "42", p, channel.Voice{Audio: []byte("OggS opus")}); err != nil {
		t.Fatal(err)
	}
	if len(f.sent) != 3 {
		t.Fatalf("sent %d, want 3: %v", len(f.sent), f.sent)
	}
	v := f.sent[0]
	if v["method"] != "sendVoice" || v["chat_id"] != "42" || v["voice"] != "OggS opus" || v["voice.type"] != "audio/ogg" ||
		v["caption"] != "<b>Pip &amp; co</b>" || v["parse_mode"] != "HTML" {
		t.Fatalf("voice note: %v", v)
	}
	if au := f.sent[1]; au["method"] != "sendAudio" || au["audio"] != "ID3 mp3" || au["audio.name"] != "voice.mp3" ||
		au["audio.type"] != "audio/mpeg" || au["title"] != "Pip & co" {
		t.Fatalf("audio file: %v", au)
	}
	if fb := f.sent[2]; fb["method"] != "sendAudio" || fb["audio"] != "OggS opus" || fb["parse_mode"] != nil || fb["caption"] != "Pip & co" {
		t.Fatalf("fallback for blocked voice messages: %v", fb)
	}
	if a.SpeechFormat() != "opus" {
		t.Fatalf("Telegram voice notes are Ogg Opus, asked for %q", a.SpeechFormat())
	}
}

func TestSendVoiceErrorsNeverContainToken(t *testing.T) {
	a := New(token, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	a.BaseURL = "http://127.0.0.1:1" // nothing listens
	err := a.SendVoice(context.Background(), "42", channel.Persona{}, channel.Voice{Audio: []byte("OggS")})
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("error leaks the token or is nil: %v", err)
	}
}
