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
	ids, err := a.SendVoice(ctx, "42", p, channel.Voice{Audio: []byte("OggS opus"), Format: "ogg"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != "2" {
		t.Fatalf("voice note ids: %v", ids)
	}
	// MP3 is an audio file.
	if _, err := a.SendVoice(ctx, "42", p, channel.Voice{Audio: []byte("ID3 mp3"), Format: "mp3"}); err != nil {
		t.Fatal(err)
	}
	// Formats sendAudio doesn't take are a document.
	if _, err := a.SendVoice(ctx, "42", p, channel.Voice{Audio: []byte("RIFF wav"), Format: "wav"}); err != nil {
		t.Fatal(err)
	}
	// Someone who blocks voice messages gets the note as a file.
	f.noVoice = true
	if _, err := a.SendVoice(ctx, "42", p, channel.Voice{Audio: []byte("OggS opus")}); err != nil {
		t.Fatal(err)
	}
	if len(f.sent) != 4 {
		t.Fatalf("sent %d, want 4: %v", len(f.sent), f.sent)
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
	if doc := f.sent[2]; doc["method"] != "sendDocument" || doc["document"] != "RIFF wav" || doc["document.name"] != "voice.wav" ||
		doc["document.type"] != "audio/wav" {
		t.Fatalf("wav as a document: %v", doc)
	}
	if fb := f.sent[3]; fb["method"] != "sendDocument" || fb["document"] != "OggS opus" || fb["document.name"] != "voice.ogg" ||
		fb["parse_mode"] != nil || fb["caption"] != "Pip & co" {
		t.Fatalf("fallback for blocked voice messages: %v", fb)
	}
	if a.SpeechFormat() != "opus" {
		t.Fatalf("Telegram voice notes are Ogg Opus, asked for %q", a.SpeechFormat())
	}
}

func TestSendVoiceErrorsNeverContainToken(t *testing.T) {
	a := New(token, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	a.BaseURL = "http://127.0.0.1:1" // nothing listens
	_, err := a.SendVoice(context.Background(), "42", channel.Persona{}, channel.Voice{Audio: []byte("OggS")})
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("error leaks the token or is nil: %v", err)
	}
}
