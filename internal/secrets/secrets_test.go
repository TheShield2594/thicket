package secrets

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/theshield2594/thicket/internal/store"
)

func TestRoundTripAndRedaction(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	key, err := LoadKey("", dir)
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "secret.key")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode %v", fi.Mode().Perm())
	}
	red := NewRedactor()
	s, _ := New(db, key, red)
	if err := s.Put(ctx, "ha_token", "super-secret-token-value"); err != nil {
		t.Fatal(err)
	}
	if v, err := s.Get(ctx, "ha_token"); err != nil || v != "super-secret-token-value" {
		t.Fatalf("got %q %v", v, err)
	}
	// Ciphertext must not contain the plaintext.
	_, ct, _ := db.SecretBlob(ctx, "ha_token")
	if bytes.Contains(ct, []byte("super-secret")) {
		t.Fatal("secret stored in plaintext")
	}
	out, err := s.Expand(ctx, "Bearer {{secret:ha_token}}")
	if err != nil || out != "Bearer super-secret-token-value" {
		t.Fatalf("expand: %q %v", out, err)
	}
	if _, err := s.Expand(ctx, "{{secret:missing}}"); err == nil {
		t.Fatal("missing secret should error")
	}
	if got := red.Redact("token=super-secret-token-value"); strings.Contains(got, "super-secret") {
		t.Fatalf("not redacted: %s", got)
	}
	if got := red.Redact("key sk-or-v1-abcdef0123456789abcdef0123456789"); strings.Contains(got, "abcdef0123") {
		t.Fatalf("pattern not redacted: %s", got)
	}

	// A different key can't read it.
	other, _ := New(db, bytes.Repeat([]byte{1}, 32), NewRedactor())
	if _, err := other.Get(ctx, "ha_token"); err == nil {
		t.Fatal("decrypted with wrong key")
	}
}

func TestLogHandlerRedacts(t *testing.T) {
	red := NewRedactor()
	red.Add("hunter2-hunter2")
	var buf bytes.Buffer
	log := slog.New(NewLogHandler(slog.NewJSONHandler(&buf, nil), red))
	log.Info("calling with hunter2-hunter2", "auth", "Bearer hunter2-hunter2", "err", os.ErrNotExist,
		"raw", json.RawMessage(`{"token":"hunter2-hunter2"}`), "map", map[string]string{"k": "hunter2-hunter2"})
	if strings.Contains(buf.String(), "hunter2") {
		t.Fatalf("secret leaked to log: %s", buf.String())
	}
}

func TestEnvFallbackWhenStoredCopyUnreadable(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, _ := store.Open(ctx, filepath.Join(dir, "t.db"))
	defer db.Close()
	a, _ := New(db, bytes.Repeat([]byte{1}, 32), NewRedactor())
	a.Put(ctx, OpenRouterKey, "stored-key-value-123")
	b, _ := New(db, bytes.Repeat([]byte{2}, 32), NewRedactor()) // key changed
	b.SetEnvFallback(OpenRouterKey, "env-key-value-456")
	if v, err := b.Get(ctx, OpenRouterKey); err != nil || v != "env-key-value-456" {
		t.Fatalf("got %q %v", v, err)
	}
	a.Put(ctx, "zz_other", "another-secret-789")
	db.PutSecretBlob(ctx, "aa_broken", []byte("123456789012"), []byte("garbage"))
	red := NewRedactor()
	c, _ := New(db, bytes.Repeat([]byte{1}, 32), red)
	if err := c.WarmRedactor(ctx); err == nil {
		t.Fatal("expected an error for the broken secret")
	}
	if strings.Contains(red.Redact("x another-secret-789"), "another") {
		t.Fatal("secrets after a broken one were not warmed")
	}
}

func TestChatTokenShapes(t *testing.T) {
	for _, s := range []string{
		"my bot is 123456789:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw4 ok",
		"https://example.com/hooks/thk_abcdefghijklmnopqrstuvwxyzABCDEF",
	} {
		if !LooksLikeSecret(s) {
			t.Errorf("not caught: %s", s)
		}
	}
	for _, s := range []string{"meet at 12:30", "order 1234567:aa"} {
		if LooksLikeSecret(s) {
			t.Errorf("false positive: %s", s)
		}
	}
}
