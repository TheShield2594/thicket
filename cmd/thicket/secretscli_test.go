package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/theshield2594/thicket/internal/secrets"
	"github.com/theshield2594/thicket/internal/store"
)

func secretsEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for k, v := range map[string]string{"THICKET_DATA_DIR": dir, "THICKET_SECRET_KEY": "", "THICKET_SECRET_KEY_FILE": "", "DISCORD_TOKEN": ""} {
		old, had := os.LookupEnv(k)
		os.Setenv(k, v)
		t.Cleanup(func() {
			if had {
				os.Setenv(k, old)
			} else {
				os.Unsetenv(k)
			}
		})
	}
	return dir
}

func run(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := secretsCmd(args, strings.NewReader(stdin), &out)
	return out.String(), err
}

func readSecret(t *testing.T, dir, keyEnv, name string) (string, error) {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(dir, "thicket.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	key, _, err := secrets.ResolveKey(keyEnv, "", dir)
	if err != nil {
		t.Fatal(err)
	}
	sec, _ := secrets.New(db, key, secrets.NewRedactor())
	return sec.Get(context.Background(), name)
}

func TestSecretsSetListRotate(t *testing.T) {
	dir := secretsEnv(t)
	if _, err := run(t, "", "set", "Bad-Name"); err == nil {
		t.Fatal("a bad name was accepted")
	}
	if _, err := run(t, "  \n", "set", "discord_token"); err == nil {
		t.Fatal("an empty value was accepted")
	}
	out, err := run(t, "discord-token-value-123\n", "set", "discord_token")
	if err != nil || !strings.Contains(out, "Stored discord_token") || !strings.Contains(out, "Restart Thicket") {
		t.Fatalf("set: %q %v", out, err)
	}
	if strings.Contains(out, "discord-token-value-123") {
		t.Fatal("set printed the value")
	}
	run(t, "gh-value-4567890", "set", "github_token")

	os.Setenv("DISCORD_TOKEN", "from-env-value-999")
	out, err = run(t, "", "list")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"discord_token", "also in the environment (DISCORD_TOKEN)", "github_token"} {
		if !strings.Contains(out, want) {
			t.Errorf("list missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "value") {
		t.Fatalf("list shows values:\n%s", out)
	}
	os.Setenv("DISCORD_TOKEN", "")

	oldKey, _ := os.ReadFile(filepath.Join(dir, "secret.key"))
	out, err = run(t, "", "rotate-key")
	if err != nil || !strings.Contains(out, "Re-encrypted 2 secret(s)") {
		t.Fatalf("rotate: %q %v", out, err)
	}
	newKey, _ := os.ReadFile(filepath.Join(dir, "secret.key"))
	if bytes.Equal(oldKey, newKey) {
		t.Fatal("the key didn't change")
	}
	if kept, _ := os.ReadFile(filepath.Join(dir, "secret.key.old")); !bytes.Equal(kept, oldKey) {
		t.Fatal("the old key wasn't kept aside")
	}
	if v, err := readSecret(t, dir, "", "github_token"); err != nil || v != "gh-value-4567890" {
		t.Fatalf("after rotation: %q %v", v, err)
	}

	out, err = run(t, "", "delete", "github_token")
	if err != nil || !strings.Contains(out, "Deleted github_token") {
		t.Fatalf("delete: %q %v", out, err)
	}
	if _, err := readSecret(t, dir, "", "github_token"); err == nil {
		t.Fatal("still there after delete")
	}
}

func TestRotateKeyFromEnvNeedsOut(t *testing.T) {
	dir := secretsEnv(t)
	os.Setenv("THICKET_SECRET_KEY", "an env master key")
	run(t, "v-1234567890", "set", "github_token")
	if _, err := run(t, "", "rotate-key"); err == nil || !strings.Contains(err.Error(), "--out") {
		t.Fatalf("rotate without --out: %v", err)
	}
	newPath := filepath.Join(dir, "new.key")
	out, err := run(t, "", "rotate-key", "--out", newPath)
	if err != nil || !strings.Contains(out, "put its contents in THICKET_SECRET_KEY") {
		t.Fatalf("rotate --out: %q %v", out, err)
	}
	b, _ := os.ReadFile(newPath)
	if v, err := readSecret(t, dir, strings.TrimSpace(string(b)), "github_token"); err != nil || v != "v-1234567890" {
		t.Fatalf("with the new key: %q %v", v, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "secret.key")); err == nil {
		t.Fatal("a data-dir key file was created though the key comes from the environment")
	}
}

func TestSecretKeyFile(t *testing.T) {
	dir := secretsEnv(t)
	keyFile := filepath.Join(t.TempDir(), "thicket_secret_key")
	os.WriteFile(keyFile, []byte("docker secret master key\n"), 0o600)
	os.Setenv("THICKET_SECRET_KEY_FILE", keyFile)
	if _, err := run(t, "file-key-value-123", "set", "github_token"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "secret.key")); err == nil {
		t.Fatal("secret.key was created though THICKET_SECRET_KEY_FILE is set")
	}
	// The file's contents are the key, the same as THICKET_SECRET_KEY.
	if v, err := readSecret(t, dir, "docker secret master key", "github_token"); err != nil || v != "file-key-value-123" {
		t.Fatalf("read with the file's key: %q %v", v, err)
	}
}
