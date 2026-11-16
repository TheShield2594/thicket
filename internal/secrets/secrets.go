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
)

var ErrNotFound = errors.New("secret not found")

var nameRe = regexp.MustCompile(`^[a-z0-9_]{2,48}$`)

// ValidName reports whether name is a usable secret name.
func ValidName(name string) bool { return nameRe.MatchString(name) }

// LoadKey returns the master key. It uses THICKET_SECRET_KEY if given
// (any string; hashed to 32 bytes), else a key file in dataDir, creating it
// on first run.
func LoadKey(fromEnv, dataDir string) ([]byte, error) {
	if fromEnv != "" {
		if raw, err := base64.StdEncoding.DecodeString(fromEnv); err == nil && len(raw) == 32 {
			return raw, nil
		}
		sum := sha256.Sum256([]byte(fromEnv))
		return sum[:], nil
	}
	path := filepath.Join(dataDir, "secret.key")
	if b, err := os.ReadFile(path); err == nil {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
		if err != nil || len(raw) != 32 {
			return nil, fmt.Errorf("%s is corrupt; restore it from backup or delete it (stored secrets will then need to be re-entered)", path)
		}
		return raw, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(key)+"\n"), 0o600); err != nil {
		return nil, err
	}
	return key, nil
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
		s.red.Add(value)
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
	s.red.Add(value)
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
	s.red.Add(v)
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

func (s *Store) Delete(ctx context.Context, name string) error { return s.db.DeleteSecret(ctx, name) }

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
var secretShapes = regexp.MustCompile(`(?i)\b(sk-or-v1-[a-z0-9]{20,}|sk-(?:ant-|proj-)?[a-z0-9_\-]{20,}|xox[abprs]-[a-z0-9\-]{10,}|gh[pousr]_[a-z0-9]{30,}|eyJ[a-z0-9_\-]{20,}\.[a-z0-9_\-]{20,}\.[a-z0-9_\-]{10,}|ntn_[a-z0-9]{30,}|secret_[a-z0-9]{30,}|thk_[a-z0-9_\-]{30,}|[0-9]{6,12}:aa[a-z0-9_\-]{33})\b`)

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
