package secrets

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func newBox(t *testing.T) *Box {
	t.Helper()
	k, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	key, err := ParseKey(k + "\n")
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(key)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSealOpen(t *testing.T) {
	b := newBox(t)
	aad := []byte("repository:1")
	pt := []byte(`{"RESTIC_PASSWORD":"s3cret"}`)

	c1, err := b.Seal(pt, aad)
	if err != nil {
		t.Fatal(err)
	}
	c2, _ := b.Seal(pt, aad)
	if bytes.Equal(c1, c2) {
		t.Fatal("same plaintext produced identical ciphertexts (nonce reuse?)")
	}
	if bytes.Contains(c1, []byte("s3cret")) {
		t.Fatal("plaintext visible in ciphertext")
	}
	got, err := b.Open(c1, aad)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, pt) {
		t.Fatalf("got %q", got)
	}
}

func TestOpenFailures(t *testing.T) {
	b := newBox(t)
	aad := []byte("repository:1")
	ct, err := b.Seal([]byte("secret"), aad)
	if err != nil {
		t.Fatal(err)
	}
	tampered := bytes.Clone(ct)
	tampered[len(tampered)-1] ^= 1

	tests := []struct {
		name string
		box  *Box
		data []byte
		aad  []byte
	}{
		{"wrong aad (secret moved to another row)", b, ct, []byte("repository:2")},
		{"wrong key", newBox(t), ct, aad},
		{"tampered", b, tampered, aad},
		{"truncated", b, ct[:10], aad},
		{"empty", b, nil, aad},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := tt.box.Open(tt.data, tt.aad); !errors.Is(err, ErrDecrypt) {
				t.Fatalf("want ErrDecrypt, got %v", err)
			}
		})
	}

	unknown := bytes.Clone(ct)
	unknown[0] = 99
	if _, err := b.Open(unknown, aad); err == nil || !strings.Contains(err.Error(), "format version") {
		t.Fatalf("want format version error, got %v", err)
	}
}

func TestParseKey(t *testing.T) {
	for _, bad := range []string{"", "not base64!", "c2hvcnQ=" /* "short" */} {
		if _, err := ParseKey(bad); err == nil {
			t.Errorf("ParseKey(%q): expected error", bad)
		}
	}
	if _, err := New(make([]byte, 16)); err == nil {
		t.Error("New accepted a 16-byte key")
	}
}
