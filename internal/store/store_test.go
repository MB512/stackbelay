package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// openTest opens a fresh database in a temp directory.
func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestOpenAppliesMigrationsOnce(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	for i := 0; i < 2; i++ { // second open must not re-apply
		s, err := Open(ctx, dbPath)
		if err != nil {
			t.Fatalf("open #%d: %v", i+1, err)
		}
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		ms, _ := loadMigrations()
		if n != len(ms) {
			t.Fatalf("open #%d: %d migrations recorded, want %d", i+1, n, len(ms))
		}
		_ = s.Close()
	}
}

func TestPragmas(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	var fk int
	var mode string
	if err := s.db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if fk != 1 || mode != "wal" {
		t.Fatalf("foreign_keys=%d journal_mode=%s", fk, mode)
	}
	// Foreign keys are actually enforced.
	_, err := s.db.ExecContext(ctx, `INSERT INTO repositories (stack_id, location, secret_enc) VALUES (999, 'x', x'00')`)
	if err == nil || !strings.Contains(err.Error(), "FOREIGN KEY") {
		t.Fatalf("expected foreign key error, got %v", err)
	}
}

func TestNewerSchemaIsRejected(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO schema_migrations VALUES (9999, '9999_future.sql', '2030-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	if _, err := Open(ctx, dbPath); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("expected newer-schema error, got %v", err)
	}
}

func TestOpenRejectsBadPath(t *testing.T) {
	for _, p := range []string{"", "/tmp/x.db?_pragma=foreign_keys(0)"} {
		if _, err := Open(context.Background(), p); err == nil {
			t.Errorf("Open(%q): expected error", p)
		}
	}
}
