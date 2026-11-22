// Package secrets encrypts small secrets (repository credentials) for
// storage in the database, using AES-256-GCM with a key that is never
// stored in the database (STACKBELAY_SECRET_KEY).
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// KeySize is the required key length in bytes (AES-256).
const KeySize = 32

// formatV1 is the first byte of every ciphertext. It allows changing the
// format (e.g. adding a key id for key rotation) without breaking old rows.
const formatV1 byte = 1

// ErrDecrypt is returned when a ciphertext cannot be decrypted: wrong key,
// wrong associated data, or tampered data. The cases are deliberately not
// distinguished.
var ErrDecrypt = errors.New("secrets: decryption failed (wrong key or corrupted data)")

// Box encrypts and decrypts with one key. It is safe for concurrent use.
type Box struct {
	aead cipher.AEAD
}

// New creates a Box for a 32-byte key.
func New(key []byte) (*Box, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("secrets: key must be %d bytes, got %d", KeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secrets: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secrets: %w", err)
	}
	return &Box{aead: aead}, nil
}

// ParseKey decodes a base64 (standard encoding) key as used in
// STACKBELAY_SECRET_KEY. Surrounding whitespace is ignored, so the value can
// come from a file with a trailing newline.
func ParseKey(s string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("secrets: key is not valid base64: %w", err)
	}
	if len(key) != KeySize {
		return nil, fmt.Errorf("secrets: key must decode to %d bytes, got %d", KeySize, len(key))
	}
	return key, nil
}

// GenerateKey returns a new random key, base64 encoded.
func GenerateKey() (string, error) {
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return "", fmt.Errorf("secrets: %w", err)
	}
	return base64.StdEncoding.EncodeToString(key), nil
}

// Seal encrypts plaintext. aad ("associated data") is not encrypted but
// authenticated: Open only succeeds with the same aad. Callers pass the
// identity of the row the secret belongs to (e.g. "repository:<stack id>"),
// so an attacker with write access to the database cannot move a secret to
// another row. Output: format byte || nonce || ciphertext+tag.
func (b *Box) Seal(plaintext, aad []byte) ([]byte, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("secrets: generating nonce: %w", err)
	}
	out := make([]byte, 0, 1+len(nonce)+len(plaintext)+b.aead.Overhead())
	out = append(out, formatV1)
	out = append(out, nonce...)
	return b.aead.Seal(out, nonce, plaintext, aad), nil
}

// Open decrypts data produced by Seal with the same aad.
func (b *Box) Open(data, aad []byte) ([]byte, error) {
	ns := b.aead.NonceSize()
	if len(data) < 1+ns+b.aead.Overhead() {
		return nil, ErrDecrypt
	}
	if data[0] != formatV1 {
		return nil, fmt.Errorf("secrets: unknown format version %d", data[0])
	}
	nonce, ct := data[1:1+ns], data[1+ns:]
	pt, err := b.aead.Open(nil, nonce, ct, aad)
	if err != nil {
		return nil, ErrDecrypt
	}
	return pt, nil
}
