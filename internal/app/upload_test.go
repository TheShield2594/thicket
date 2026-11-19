package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/theshield2594/thicket/internal/secrets"
)

type upload struct{ name, ct, body string }

func (w *webClient) postFiles(path, text string, files ...upload) (int, map[string]any) {
	w.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("text", text)
	for _, f := range files {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, f.name))
		h.Set("Content-Type", f.ct)
		p, _ := mw.CreatePart(h)
		p.Write([]byte(f.body))
	}
	mw.Close()
	req, _ := http.NewRequest("POST", w.base+path, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-Thicket", "1")
	res, err := w.c.Do(req)
	if err != nil {
		w.t.Fatal(err)
	}
	defer res.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(res.Body).Decode(&out)
	return res.StatusCode, out
}

func TestWebUploads(t *testing.T) {
	a, _, fm, ctx := setup(t)
	if err := a.Secrets.Put(ctx, secrets.OpenRouterKey, "sk-or-test"); err != nil {
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
	room := fmt.Sprintf("%d.all", owner.ID)
	path := "/api/chats/" + room + "/messages"

	// A voice message alone (no text) and a text file.
	code, out := w.postFiles(path, "", upload{"voice message.webm", "audio/webm;codecs=opus", "webm bytes"}, upload{"notes.txt", "text/plain", "Buy oat milk."})
	if code != 200 {
		t.Fatalf("upload: %d %v", code, out)
	}
	if got := out["message"].(map[string]any)["content"]; got != "🎤 voice message.webm\n📎 notes.txt" {
		t.Fatalf("shown as %q", got)
	}
	waitFor(t, "a reply", func() bool {
		msgs, _ := a.DB.WebMessages(ctx, room, 0, 10)
		return len(msgs) >= 2 && msgs[len(msgs)-1].Role == "assistant"
	})
	req := lastChatReq(fm)
	last := req.Messages[len(req.Messages)-1].Content
	for _, want := range []string{"transcribed:]\nRemind me to call Maya at five.", "Buy oat milk."} {
		if !strings.Contains(last, want) {
			t.Errorf("model didn't get %q:\n%s", want, last)
		}
	}

	// Limits: nothing at all, and too many files.
	if code, _ := w.postFiles(path, "  "); code != http.StatusBadRequest {
		t.Fatalf("empty message: %d", code)
	}
	var many []upload
	for i := 0; i < 9; i++ {
		many = append(many, upload{fmt.Sprintf("f%d.txt", i), "text/plain", "x"})
	}
	if code, out := w.postFiles(path, "hi", many...); code != http.StatusBadRequest || !strings.Contains(fmt.Sprint(out["error"]), "at most 8") {
		t.Fatalf("too many files: %d %v", code, out)
	}
	// File names can't carry a path.
	if code, out := w.postFiles(path, "", upload{"../../etc/passwd.txt", "text/plain", "x"}); code != 200 || !strings.HasSuffix(out["message"].(map[string]any)["content"].(string), "📎 passwd.txt") {
		t.Fatalf("path in name: %d %v", code, out)
	}
}
