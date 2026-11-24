package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/MB512/stackbelay/internal/secrets"
)

func boxWithKey(t *testing.T, key string) *secrets.Box {
	t.Helper()
	b, err := secrets.New([]byte(key))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

const otherKey = "fedcba9876543210fedcba9876543210"

func TestOpenRefusesWrongKey(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(ctx, dbPath, testBox(t))
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	if _, err := Open(ctx, dbPath, boxWithKey(t, otherKey)); !errors.Is(err, ErrKeyMismatch) {
		t.Fatalf("open with another key: err = %v, want ErrKeyMismatch", err)
	}
	s, err = Open(ctx, dbPath, testBox(t))
	if err != nil {
		t.Fatalf("open with the right key: %v", err)
	}
	_ = s.Close()
}

// TestKeyCheckWithoutRow covers a database without key_check row
// (simulated by deleting it), with and without an encrypted repository.
func TestKeyCheckWithoutRow(t *testing.T) {
	tests := []struct {
		name     string
		withRepo bool
		key      string
		wantErr  bool
	}{
		{"no repositories, any key", false, otherKey, false},
		{"repository, right key", true, "0123456789abcdef0123456789abcdef", false},
		{"repository, wrong key", true, otherKey, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			dbPath := filepath.Join(t.TempDir(), "test.db")
			s, err := Open(ctx, dbPath, testBox(t))
			if err != nil {
				t.Fatal(err)
			}
			if tt.withRepo {
				if _, err := s.SetRepository(ctx, stackID(t, s, "web"), "/repos/web", "pw", nil); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.db.ExecContext(ctx, `DELETE FROM meta`); err != nil {
				t.Fatal(err)
			}
			_ = s.Close()

			s, err = Open(ctx, dbPath, boxWithKey(t, tt.key))
			if tt.wantErr {
				if !errors.Is(err, ErrKeyMismatch) {
					t.Fatalf("err = %v, want ErrKeyMismatch", err)
				}
				// Nothing was written: the right key still opens it.
				s, err = Open(ctx, dbPath, testBox(t))
				if err != nil {
					t.Fatal(err)
				}
				_ = s.Close()
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var n int
			if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM meta WHERE key = 'key_check'`).Scan(&n); err != nil || n != 1 {
				t.Fatalf("key_check rows: %d, %v", n, err)
			}
			_ = s.Close()
			// From now on the check row decides.
			if _, err := Open(ctx, dbPath, boxWithKey(t, "abcdefabcdefabcdefabcdefabcdefab")); !errors.Is(err, ErrKeyMismatch) {
				t.Fatalf("after writing the check: err = %v, want ErrKeyMismatch", err)
			}
		})
	}
}
