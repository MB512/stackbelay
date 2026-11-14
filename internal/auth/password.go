// Package auth implements password hashing, sessions, the initial admin
// account and authorization checks.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters (RFC 9106, "second recommended option": 64 MiB,
// 3 passes). Stored with every hash, so they can be raised later without
// breaking existing hashes.
const (
	argonTime    = 3
	argonMemory  = 64 * 1024 // KiB
	argonThreads = 2
	argonKeyLen  = 32
	argonSaltLen = 16
)

// Password length limits for user-chosen passwords.
const (
	MinPasswordLen = 12
	MaxPasswordLen = 256
)

// ErrMismatch is returned by CheckPassword for a wrong password.
var ErrMismatch = errors.New("password does not match")

// HashPassword returns an argon2id hash in the common PHC string format:
// $argon2id$v=19$m=65536,t=3,p=2$<salt>$<hash> (base64 without padding).
func HashPassword(password string) (string, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generating salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	enc := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, argonMemory, argonTime, argonThreads, enc.EncodeToString(salt), enc.EncodeToString(key)), nil
}

// CheckPassword compares password with a hash from HashPassword. It returns
// nil on match, ErrMismatch on mismatch, and another error for a malformed
// hash.
func CheckPassword(hash, password string) error {
	parts := strings.Split(hash, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return errors.New("unsupported password hash format")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return errors.New("unsupported argon2 version")
	}
	var memory, time uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &memory, &time, &threads); err != nil ||
		memory == 0 || memory > 1<<21 || time == 0 || time > 100 || threads == 0 {
		return errors.New("invalid argon2 parameters")
	}
	enc := base64.RawStdEncoding
	salt, err := enc.DecodeString(parts[4])
	if err != nil {
		return errors.New("invalid salt in password hash")
	}
	want, err := enc.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return errors.New("invalid key in password hash")
	}
	got := argon2.IDKey([]byte(password), salt, time, memory, threads, uint32(len(want))) //nolint:gosec // G115: len(want) is small
	// Constant-time comparison, so timing does not reveal how many bytes
	// matched.
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return ErrMismatch
	}
	return nil
}

// dummyHash is checked against when a login names an unknown user, so that
// the response time does not reveal whether the user exists. It is computed
// on first use (sync.OnceValue is like Lazy<T> in C#), not at program start.
var dummyHash = sync.OnceValue(func() string {
	h, err := HashPassword("not-a-real-password-just-for-timing")
	if err != nil {
		panic(err) // only fails if the system random source is broken
	}
	return h
})

// CheckDummy spends the same time as CheckPassword on a real hash.
func CheckDummy(password string) { _ = CheckPassword(dummyHash(), password) }

// ValidateNewPassword checks a user-chosen password.
func ValidateNewPassword(password string) error {
	n := utf8.RuneCountInString(password)
	switch {
	case !utf8.ValidString(password):
		return errors.New("password is not valid UTF-8")
	case n < MinPasswordLen:
		return fmt.Errorf("password must be at least %d characters long", MinPasswordLen)
	case n > MaxPasswordLen:
		return fmt.Errorf("password must be at most %d characters long", MaxPasswordLen)
	}
	return nil
}

// generatedAlphabet has 32 characters without look-alikes (0/O, 1/l/I), so
// each character carries 5 bits.
const generatedAlphabet = "abcdefghjkmnpqrstuvwxyz23456789AB"

// GeneratePassword returns a random password of 5 groups of 4 characters
// ("k7qp-9vxm-tr2l-wb4n-hs6e"): 20 characters, 100 bits of entropy.
func GeneratePassword() (string, error) {
	buf := make([]byte, 20)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating password: %w", err)
	}
	var b strings.Builder
	for i, c := range buf {
		if i > 0 && i%4 == 0 {
			b.WriteByte('-')
		}
		// 256 is a multiple of 32, so c%32 is uniformly distributed.
		b.WriteByte(generatedAlphabet[c%32])
	}
	return b.String(), nil
}
