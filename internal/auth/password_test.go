package auth

import (
	"errors"
	"regexp"
	"strings"
	"testing"
)

func TestHashAndCheck(t *testing.T) {
	h, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "$argon2id$v=19$m=65536,t=3,p=2$") {
		t.Fatalf("unexpected format %q", h)
	}
	h2, _ := HashPassword("correct horse battery")
	if h == h2 {
		t.Fatal("same password produced the same hash (salt not random?)")
	}
	if err := CheckPassword(h, "correct horse battery"); err != nil {
		t.Fatalf("correct password rejected: %v", err)
	}
	if err := CheckPassword(h, "correct horse batterY"); !errors.Is(err, ErrMismatch) {
		t.Fatalf("want ErrMismatch, got %v", err)
	}
}

func TestCheckPasswordMalformed(t *testing.T) {
	good, _ := HashPassword("x")
	parts := strings.Split(good, "$")
	for name, h := range map[string]string{
		"empty":          "",
		"bcrypt":         "$2a$10$abcdefghijklmnopqrstuuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ012",
		"argon2i":        strings.Replace(good, "argon2id", "argon2i", 1),
		"bad version":    strings.Replace(good, "v=19", "v=16", 1),
		"huge memory":    strings.Replace(good, "m=65536", "m=99999999", 1),
		"zero time":      strings.Replace(good, "t=3", "t=0", 1),
		"bad salt":       strings.Join([]string{"", parts[1], parts[2], parts[3], "!!", parts[5]}, "$"),
		"missing fields": "$argon2id$v=19$m=65536,t=3,p=2$abc",
	} {
		err := CheckPassword(h, "x")
		if err == nil || errors.Is(err, ErrMismatch) {
			t.Errorf("%s: want format error, got %v", name, err)
		}
	}
}

func TestValidateNewPassword(t *testing.T) {
	for pw, ok := range map[string]bool{
		"short":                    false,
		"exactly12chr":             true,
		"äöüäöüäöüäöü":             true, // 12 characters, 24 bytes
		strings.Repeat("a", 256):   true,
		strings.Repeat("a", 257):   false,
		"invalid\xffutf8-password": false,
	} {
		if err := ValidateNewPassword(pw); (err == nil) != ok {
			t.Errorf("ValidateNewPassword(%q) = %v, want ok=%v", pw, err, ok)
		}
	}
}

func TestGeneratePassword(t *testing.T) {
	re := regexp.MustCompile(`^[a-hjkmnp-z2-9AB]{4}(-[a-hjkmnp-z2-9AB]{4}){4}$`)
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		p, err := GeneratePassword()
		if err != nil {
			t.Fatal(err)
		}
		if !re.MatchString(p) {
			t.Fatalf("unexpected format %q", p)
		}
		if seen[p] {
			t.Fatal("duplicate generated password")
		}
		seen[p] = true
	}
}

func TestCheckDummy(t *testing.T) {
	CheckDummy("anything") // must not panic
}
