// Package auth holds Thicket's sign-in building blocks: password hashing
// (argon2id) and a small OpenID Connect client. Sessions and accounts live
// in the store; the web app wires them together.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters: OWASP's minimum (19 MiB, 2 passes, 1 lane). Each
// hash holds the memory while it runs, so concurrent hashing is bounded
// too (hashSlots).
const (
	argonTime    = 2
	argonMemory  = 19 * 1024 // KiB
	argonThreads = 1
	argonKeyLen  = 32
	saltLen      = 16

	MinPasswordLen = 10
	MaxPasswordLen = 256
)

// hashSlots bounds how many hashes run at once (19 MiB each).
var hashSlots = make(chan struct{}, 4)

// ErrMismatch means the password doesn't match the hash.
var ErrMismatch = errors.New("wrong password")

// CheckPasswordRules says what's wrong with a new password, or nil.
func CheckPasswordRules(pw string) error {
	n := utf8.RuneCountInString(pw)
	switch {
	case n < MinPasswordLen:
		return fmt.Errorf("use at least %d characters (a few words make a good one)", MinPasswordLen)
	case len(pw) > MaxPasswordLen:
		return fmt.Errorf("that's too long (at most %d bytes)", MaxPasswordLen)
	case strings.TrimSpace(pw) != pw:
		return errors.New("it starts or ends with a space, which is easy to get wrong; leave those off")
	}
	return nil
}

// HashPassword returns an argon2id hash in the usual PHC string form.
func HashPassword(pw string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	hashSlots <- struct{}{}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	<-hashSlots
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory, argonTime, argonThreads,
		b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// VerifyPassword checks pw against a hash from HashPassword (with the
// parameters stored in it). An empty hash never matches, but still costs
// a hash, so a missing account takes as long as a wrong password.
func VerifyPassword(hash, pw string) error {
	m, t, p, salt, want, err := parseHash(hash)
	if err != nil {
		m, t, p, salt, want = argonMemory, argonTime, argonThreads, make([]byte, saltLen), make([]byte, argonKeyLen)
	}
	if len(pw) > MaxPasswordLen {
		pw = pw[:MaxPasswordLen]
	}
	hashSlots <- struct{}{}
	got := argon2.IDKey([]byte(pw), salt, t, m, p, uint32(len(want)))
	<-hashSlots
	if err != nil || subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrMismatch
	}
	return nil
}

func parseHash(h string) (m, t uint32, p uint8, salt, key []byte, err error) {
	parts := strings.Split(h, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return 0, 0, 0, nil, nil, errors.New("not an argon2id hash")
	}
	var v int
	if _, err = fmt.Sscanf(parts[2], "v=%d", &v); err != nil || v != argon2.Version {
		return 0, 0, 0, nil, nil, errors.New("unsupported argon2 version")
	}
	if _, err = fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return 0, 0, 0, nil, nil, errors.New("bad argon2 parameters")
	}
	// Refuse parameters that would let a planted hash exhaust memory.
	if m == 0 || m > 256*1024 || t == 0 || t > 10 || p == 0 || p > 8 {
		return 0, 0, 0, nil, nil, errors.New("argon2 parameters out of range")
	}
	b64 := base64.RawStdEncoding
	if salt, err = b64.DecodeString(parts[4]); err != nil {
		return 0, 0, 0, nil, nil, err
	}
	if key, err = b64.DecodeString(parts[5]); err != nil || len(key) < 16 || len(key) > 64 {
		return 0, 0, 0, nil, nil, errors.New("bad argon2 key")
	}
	return m, t, p, salt, key, nil
}
