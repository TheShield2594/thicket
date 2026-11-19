package agent

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/theshield2594/thicket/internal/character"
	"github.com/theshield2594/thicket/internal/config"
	"github.com/theshield2594/thicket/internal/store"
)

func TestMoodsFadeAndApprovalsWait(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	e := &Engine{Now: func() time.Time { return now }}
	pip := &character.Character{ID: "pip"}
	e.noteReply(&turn{char: pip, conv: &store.Conversation{ID: 7}}, true, "")
	if m := e.sprigMood("pip"); m != "pleased" {
		t.Fatalf("landed reply: %q", m)
	}
	now = now.Add(time.Minute)
	e.noteReply(&turn{char: pip, conv: &store.Conversation{ID: 8}}, true, "Unsure")
	if v, ok := e.LastVerdict("pip", 8); !ok || !v.InCharacter || v.Mood != "unsure" || e.sprigMood("pip") != "unsure" {
		t.Fatalf("refusal: %+v %q", v, e.sprigMood("pip"))
	}
	if v, _ := e.LastVerdict("pip", 7); v.Mood != "pleased" {
		t.Fatal("verdicts are kept per conversation")
	}
	now = now.Add(moodDecay)
	if m := e.sprigMood("pip"); m != "" {
		t.Fatalf("mood should have faded: %q", m)
	}
	// Waiting on an approval is unsure, whatever the reply said, until
	// it's answered.
	e.noteWaiting("pip")
	e.noteReply(&turn{char: pip}, true, "pleased")
	if m := e.sprigMood("pip"); m != "unsure" {
		t.Fatalf("waiting: %q", m)
	}
	e.doneWaiting("pip")
	if m := e.sprigMood("pip"); m != "pleased" {
		t.Fatalf("answered: %q", m)
	}
}

func TestPersonaPicksMoodPicture(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"pip.png", "pip.unsure.png"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("\x89PNG"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	e := &Engine{Now: func() time.Time { return now }, Cfg: &config.Config{PublicURL: "https://thicket.example"},
		Chars: character.NewRegistry(dir, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))}
	pip := &character.Character{ID: "pip", Name: "Pip", Avatar: "pip.png"}
	if u := e.persona(pip).AvatarURL; u != "https://thicket.example/avatars/pip.png" {
		t.Fatalf("no mood: %s", u)
	}
	e.noteReply(&turn{char: pip}, false, "")
	if u := e.persona(pip).AvatarURL; u != "https://thicket.example/avatars/pip.unsure.png" {
		t.Fatalf("unsure: %s", u)
	}
	// No pleased picture: the usual one.
	e.noteReply(&turn{char: pip}, true, "")
	if u := e.persona(pip).AvatarURL; u != "https://thicket.example/avatars/pip.png" {
		t.Fatalf("pleased without a picture: %s", u)
	}
}
