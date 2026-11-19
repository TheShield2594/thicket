package app

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/theshield2594/thicket/internal/secrets"
	"github.com/theshield2594/thicket/internal/store"
)

// In the web app a voice message gets a reply with an audio player: the
// audio is attached to the reply and served to its owner only.
func TestWebAppSpokenReplies(t *testing.T) {
	a, _, _, ctx := setup(t)
	if err := os.WriteFile(filepath.Join(a.Cfg.CharactersDir, "pip.md"), []byte(pipSpeaks), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.Chars.Load(); err != nil {
		t.Fatal(err)
	}
	if err := a.Secrets.Put(ctx, secrets.OpenRouterKey, "sk-or-test"); err != nil {
		t.Fatal(err)
	}
	speech := &speechServer{}
	speechSrv := httptest.NewServer(speech.handler(t))
	defer speechSrv.Close()
	if err := a.Settings.Set(ctx, "speech_url", speechSrv.URL+"/v1"); err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	go a.Web.Run(runCtx, a.Engine.HandleIncoming)
	waitFor(t, "web adapter", a.Web.Connected)
	srv := httptest.NewServer(a.Web.Handler())
	defer srv.Close()
	jar, _ := cookiejar.New(nil)
	w := &webClient{t: t, base: srv.URL, c: &http.Client{Jar: jar}}
	token, _ := a.DB.CreateWebLink(ctx, 0, time.Hour)
	if code, out := w.do("POST", "/api/login", map[string]string{"token": token}, true); code != 200 {
		t.Fatalf("login: %d %v", code, out)
	}
	owner, _ := a.DB.Owner(ctx)
	chat := fmt.Sprintf("%d.pip", owner.ID)

	if code, out := w.postFiles("/api/chats/"+chat+"/messages", "", upload{"voice message.webm", "audio/webm", "webm bytes"}); code != 200 {
		t.Fatalf("upload: %d %v", code, out)
	}
	var msgs []store.WebMessage
	waitFor(t, "a spoken reply", func() bool {
		msgs, _ = a.DB.WebMessages(ctx, chat, 0, 10)
		return len(msgs) == 2 && msgs[1].Audio
	})
	if msgs[1].Character != "pip" || msgs[1].Content != "Pip here! I'll sort your calendar." {
		t.Fatalf("reply: %+v", msgs[1])
	}
	if reqs := speech.take(); len(reqs) != 1 || reqs[0]["voice"] != "nova" || reqs[0]["response_format"] != "mp3" {
		t.Fatalf("speech: %v", reqs)
	}
	res, err := w.c.Get(srv.URL + "/api/chats/" + chat + "/messages/" + strconv.FormatInt(msgs[1].ID, 10) + "/audio")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || string(body) != "OggS voice=nova" || res.Header.Get("Content-Type") != "audio/ogg" {
		t.Fatalf("audio: %d %q %q", res.StatusCode, body, res.Header.Get("Content-Type"))
	}
	// Signed out, or someone else's chat: no audio.
	if res, _ := http.Get(srv.URL + "/api/chats/" + chat + "/messages/" + strconv.FormatInt(msgs[1].ID, 10) + "/audio"); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("signed out: %d", res.StatusCode)
	}

	// The brief/full toggle.
	if code, _ := w.do("PUT", "/api/sprigs/pip/voice", map[string]string{"mode": "loud"}, true); code != http.StatusBadRequest {
		t.Fatalf("bad mode: %d", code)
	}
	if code, out := w.do("PUT", "/api/sprigs/pip/voice", map[string]string{"mode": "brief"}, true); code != 200 || out["voice_mode"] != "brief" {
		t.Fatalf("set brief: %d %v", code, out)
	}
	_, me := w.do("GET", "/api/me", nil, false)
	for _, s := range me["sprigs"].([]any) {
		if v := s.(map[string]any); v["id"] == "pip" && (v["voice_mode"] != "brief" || v["speaks"] != "when_spoken_to") {
			t.Fatalf("pip's voice: %v", v)
		}
	}
}
