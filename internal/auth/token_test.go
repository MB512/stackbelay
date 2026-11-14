package auth

import (
	"testing"
	"time"
)

func TestTokens(t *testing.T) {
	a, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewToken()
	if len(a) != 43 || a == b {
		t.Fatalf("tokens %q %q", a, b)
	}
	h1, h2 := HashToken(a), HashToken(a)
	if h1 == a || len(h1) != 64 || h1 != h2 || h1 == HashToken(b) {
		t.Fatal("HashToken not a stable sha256 hex")
	}
	if !TokensEqual(a, a) || TokensEqual(a, b) || TokensEqual(a, "") {
		t.Fatal("TokensEqual wrong")
	}
}

func TestLoginLimiter(t *testing.T) {
	now := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	l := &LoginLimiter{FreeFailures: 3, BaseDelay: time.Second, MaxDelay: 10 * time.Second, Now: func() time.Time { return now }}

	for i := 0; i < 3; i++ { // free failures
		if ok, _ := l.Allow("admin"); !ok {
			t.Fatalf("locked after %d failures", i)
		}
		l.Failure("admin")
	}
	if ok, _ := l.Allow("admin"); !ok {
		t.Fatal("locked after only the free failures")
	}

	// Exponential: 1s, 2s, 4s, 8s, then capped at 10s.
	for i, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 10 * time.Second, 10 * time.Second} {
		l.Failure("admin")
		ok, wait := l.Allow("admin")
		if ok || wait != want {
			t.Fatalf("failure %d: ok=%v wait=%v, want %v", i+4, ok, wait, want)
		}
		now = now.Add(wait) // wait it out
	}
	if ok, _ := l.Allow("other"); !ok {
		t.Fatal("other user affected")
	}

	l.Success("admin")
	if ok, _ := l.Allow("admin"); !ok {
		t.Fatal("not reset after success")
	}

	// Old failures are forgotten after a quiet period.
	for i := 0; i < 3; i++ {
		l.Failure("bob")
	}
	now = now.Add(time.Hour)
	l.Failure("bob")
	if ok, _ := l.Allow("bob"); !ok {
		t.Fatal("old failures still counted")
	}
}

func TestLoginLimiterBounded(t *testing.T) {
	now := time.Now()
	l := &LoginLimiter{Now: func() time.Time { return now }}
	for i := 0; i < maxLimiterEntries+100; i++ {
		l.Failure(string(rune('a'+i%26)) + time.Duration(i).String())
	}
	if len(l.entries) > maxLimiterEntries {
		t.Fatalf("limiter grew to %d entries", len(l.entries))
	}
}
