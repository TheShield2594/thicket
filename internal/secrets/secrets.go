// Package secrets keeps API keys and tokens encrypted at rest (AES-256-GCM)
// and makes sure their values never reach logs or model prompts.
package secrets

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/theshield2594/thicket/internal/store"
)

// Well-known secret names.
const (
	OpenRouterKey = "openrouter_api_key"
	// ModelAPIKey is the optional key for a self-hosted model server.
	ModelAPIKey = "model_api_key"
	// Chat app bot tokens.
	DiscordToken  = "discord_token"
	TelegramToken = "telegram_token"
)

// EnvSecret is a secret that may be supplied through the environment.
type EnvSecret struct {
	Name  string // stored name
	Env   string // environment variable
	Value string // its value ("" if unset)
}

var ErrNotFound = errors.New("secret not found")

// notSecret are stored with the secrets (they belong with the one that
// is) but aren't secret, so they aren't redacted: an issuer URL or a
// client id like "thicket" would otherwise vanish from every text.
var notSecret = map[string]bool{"oidc_issuer": true, "oidc_client_id": true}

func (s *Store) learn(name, value string) {
	if !notSecret[name] {
		s.red.Add(value)
	}
}

var nameRe = regexp.MustCompile(`^[a-z0-9_]{2,48}$`)

// ValidName reports whether name is a usable secret name.
func ValidName(name string) bool { return nameRe.MatchString(name) }

// KeySource says where the master key came from.
type KeySource int

const (
	KeyFromDataDir KeySource = iota // <data dir>/secret.key, created on first run
	KeyFromEnv                      // THICKET_SECRET_KEY
	KeyFromFile                     // THICKET_SECRET_KEY_FILE (Docker secrets)
)

// KeyPath is the key file Thicket creates in the data directory.
func KeyPath(dataDir string) string { return filepath.Join(dataDir, "secret.key") }

// LoadKey returns the master key. It uses THICKET_SECRET_KEY if given
// (any string; hashed to 32 bytes), else a key file in dataDir, creating it
// on first run.
func LoadKey(fromEnv, dataDir string) ([]byte, error) {
	k, _, err := ResolveKey(fromEnv, "", dataDir)
	return k, err
}

// ResolveKey returns the master key and where it came from: the
// THICKET_SECRET_KEY value, else the file THICKET_SECRET_KEY_FILE names,
// else secret.key in dataDir, created on first run. A value that isn't
// base64 for 32 bytes is hashed to a key.
func ResolveKey(fromEnv, fromFile, dataDir string) ([]byte, KeySource, error) {
	if fromEnv != "" {
		return keyFrom(fromEnv), KeyFromEnv, nil
	}
	if fromFile != "" {
		b, err := os.ReadFile(fromFile)
		if err != nil {
			return nil, KeyFromFile, fmt.Errorf("THICKET_SECRET_KEY_FILE: %w", err)
		}
		v := strings.TrimSpace(string(b))
		if v == "" {
			return nil, KeyFromFile, fmt.Errorf("THICKET_SECRET_KEY_FILE %s is empty", fromFile)
		}
		return keyFrom(v), KeyFromFile, nil
	}
	path := KeyPath(dataDir)
	if b, err := os.ReadFile(path); err == nil {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
		if err != nil || len(raw) != 32 {
			return nil, KeyFromDataDir, fmt.Errorf("%s is corrupt; restore it from backup or delete it (stored secrets will then need to be re-entered)", path)
		}
		return raw, KeyFromDataDir, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, KeyFromDataDir, err
	}
	key, err := NewKey()
	if err != nil {
		return nil, KeyFromDataDir, err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, KeyFromDataDir, err
	}
	if err := WriteKeyFile(path, key); err != nil {
		return nil, KeyFromDataDir, err
	}
	return key, KeyFromDataDir, nil
}

func keyFrom(v string) []byte {
	if raw, err := base64.StdEncoding.DecodeString(v); err == nil && len(raw) == 32 {
		return raw
	}
	sum := sha256.Sum256([]byte(v))
	return sum[:]
}

// NewKey returns a fresh random master key.
func NewKey() ([]byte, error) {
	key := make([]byte, 32)
	_, err := rand.Read(key)
	return key, err
}

// EncodeKey is a key as it's written to a key file or an env variable.
func EncodeKey(key []byte) string { return base64.StdEncoding.EncodeToString(key) }

// WriteKeyFile writes a key file readable only by its owner, replacing
// any file there in one step.
func WriteKeyFile(path string, key []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(EncodeKey(key)+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Store is an encrypted key/value store on top of the database.
type Store struct {
	db   *store.Store
	aead cipher.AEAD
	red  *Redactor
	env  map[string]string
}

func New(db *store.Store, key []byte, red *Redactor) (*Store, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Store{db: db, aead: aead, red: red, env: map[string]string{}}, nil
}

// SetEnvFallback registers a value supplied through the environment. Stored
// secrets take precedence so a key changed in chat wins.
func (s *Store) SetEnvFallback(name, value string) {
	if value != "" {
		s.env[name] = value
		s.learn(name, value)
	}
}

func (s *Store) Put(ctx context.Context, name, value string) error {
	if !ValidName(name) {
		return fmt.Errorf("invalid secret name %q (use lowercase letters, digits, underscores)", name)
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("empty secret")
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	ct := s.aead.Seal(nil, nonce, []byte(value), []byte(name))
	if err := s.db.PutSecretBlob(ctx, name, nonce, ct); err != nil {
		return err
	}
	s.learn(name, value)
	return nil
}

func (s *Store) Get(ctx context.Context, name string) (string, error) {
	nonce, ct, err := s.db.SecretBlob(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		if v, ok := s.env[name]; ok {
			return v, nil
		}
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	pt, err := s.aead.Open(nil, nonce, ct, []byte(name))
	if err != nil {
		// An unreadable stored copy shouldn't hide a working env value.
		if v, ok := s.env[name]; ok {
			return v, nil
		}
		return "", fmt.Errorf("secret %q can't be decrypted (was the secret key changed?)", name)
	}
	v := string(pt)
	s.learn(name, v)
	return v, nil
}

func (s *Store) Has(ctx context.Context, name string) bool {
	_, err := s.Get(ctx, name)
	return err == nil
}

// Names lists stored and env-provided secret names (never values).
func (s *Store) Names(ctx context.Context) ([]string, error) {
	names, err := s.db.SecretNames(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, n := range names {
		seen[n] = true
	}
	for n := range s.env {
		if !seen[n] {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names, nil
}

// Redact scrubs secret values from text.
func (s *Store) Redact(text string) string { return s.red.Redact(text) }

// Remember redacts v from now on without storing it, for a value that is
// shown before it is saved.
func (s *Store) Remember(v string) {
	if v = strings.TrimSpace(v); v != "" {
		s.red.Add(v)
	}
}

func (s *Store) Delete(ctx context.Context, name string) error { return s.db.DeleteSecret(ctx, name) }

// Stored reports whether name is in the encrypted store (not just the
// environment).
func (s *Store) Stored(ctx context.Context, name string) (bool, error) {
	_, _, err := s.db.SecretBlob(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// ImportEnv stores each secret given through the environment, so it can
// be removed from there. The environment wins at startup: a stored copy
// that differs is replaced. It returns the names that were added or
// changed. The values are also kept as a fallback for this run.
func (s *Store) ImportEnv(ctx context.Context, items []EnvSecret) ([]string, error) {
	var changed []string
	var errs []error
	for _, it := range items {
		if strings.TrimSpace(it.Value) == "" {
			continue
		}
		s.SetEnvFallback(it.Name, strings.TrimSpace(it.Value))
		cur, err := s.getStored(ctx, it.Name)
		if err == nil && cur == strings.TrimSpace(it.Value) {
			continue
		}
		if err := s.Put(ctx, it.Name, it.Value); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", it.Env, err))
			continue
		}
		changed = append(changed, it.Name)
	}
	return changed, errors.Join(errs...)
}

// getStored reads only the encrypted store, ignoring the environment.
func (s *Store) getStored(ctx context.Context, name string) (string, error) {
	nonce, ct, err := s.db.SecretBlob(ctx, name)
	if err != nil {
		return "", err
	}
	pt, err := s.aead.Open(nil, nonce, ct, []byte(name))
	if err != nil {
		return "", fmt.Errorf("secret %q can't be decrypted", name)
	}
	return string(pt), nil
}

// RotateKey re-encrypts every stored secret under newKey, all or nothing.
// It returns how many secrets were re-encrypted. The Store keeps using
// the old key: reopen it with the new one afterwards.
func (s *Store) RotateKey(ctx context.Context, newKey []byte) (int, error) {
	next, err := New(s.db, newKey, NewRedactor())
	if err != nil {
		return 0, err
	}
	names, err := s.db.SecretNames(ctx)
	if err != nil {
		return 0, err
	}
	blobs := map[string][2][]byte{}
	for _, n := range names {
		v, err := s.getStored(ctx, n)
		if err != nil {
			return 0, fmt.Errorf("%w; nothing was changed (fix or delete it first: thicket secrets delete %s)", err, n)
		}
		nonce := make([]byte, next.aead.NonceSize())
		if _, err := rand.Read(nonce); err != nil {
			return 0, err
		}
		blobs[n] = [2][]byte{nonce, next.aead.Seal(nil, nonce, []byte(v), []byte(n))}
	}
	if err := s.db.ReplaceSecretBlobs(ctx, blobs); err != nil {
		return 0, err
	}
	return len(blobs), nil
}

// WarmRedactor decrypts every stored secret once so the redactor knows them.
func (s *Store) WarmRedactor(ctx context.Context) error {
	names, err := s.db.SecretNames(ctx)
	if err != nil {
		return err
	}
	// Keep going past failures so every readable secret is still redacted.
	var errs []error
	for _, n := range names {
		if _, err := s.Get(ctx, n); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

var placeholderRe = regexp.MustCompile(`\{\{secret:([a-z0-9_]+)\}\}`)

// Expand replaces {{secret:NAME}} placeholders with secret values.
func (s *Store) Expand(ctx context.Context, in string) (string, error) {
	var firstErr error
	out := placeholderRe.ReplaceAllStringFunc(in, func(m string) string {
		name := placeholderRe.FindStringSubmatch(m)[1]
		v, err := s.Get(ctx, name)
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("secret %q is not set", name)
		}
		return v
	})
	return out, firstErr
}

// Redactor scrubs known secret values and secret-shaped strings from text.
type Redactor struct {
	mu     sync.RWMutex
	values []string
}

func NewRedactor() *Redactor { return &Redactor{} }

func (r *Redactor) Add(v string) {
	if len(v) < 8 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, x := range r.values {
		if x == v {
			return
		}
	}
	r.values = append(r.values, v)
}

// Patterns that look like credentials even if we've never seen the value.
var secretShapes = regexp.MustCompile(`(?i)\b(sk-or-v1-[a-z0-9]{20,}|sk-(?:ant-|proj-)?[a-z0-9_\-]{20,}|xox[abprs]-[a-z0-9\-]{10,}|gh[pousr]_[a-z0-9]{30,}|eyJ[a-z0-9_\-]{20,}\.[a-z0-9_\-]{20,}\.[a-z0-9_\-]{10,}|ntn_[a-z0-9]{30,}|secret_[a-z0-9]{30,}|thk_[a-z0-9_\-]{30,})\b` +
	// A Telegram bot token has a fixed length and may end in "-", where a
	// trailing \b can't match, so it gets no trailing boundary.
	`|\b[0-9]{6,12}:aa[a-z0-9_\-]{33}` +
	// A Discord bot token (the bot's id in base64, a timestamp, a
	// signature), which may also end in "-".
	`|\b[mno][a-z0-9_\-]{22,27}\.[a-z0-9_\-]{6,7}\.[a-z0-9_\-]{27,}`)

// LooksLikeSecret reports whether s contains a credential-shaped token.
func LooksLikeSecret(s string) bool { return secretShapes.MatchString(s) }

func (r *Redactor) Redact(s string) string {
	if s == "" {
		return s
	}
	r.mu.RLock()
	for _, v := range r.values {
		if strings.Contains(s, v) {
			s = strings.ReplaceAll(s, v, "[redacted]")
		}
	}
	r.mu.RUnlock()
	return secretShapes.ReplaceAllString(s, "[redacted]")
}
