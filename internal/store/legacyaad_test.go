package store

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"
)

// legacyDB creates a database as StackVault 0.4 left it: two repositories
// and the key check sealed with the old associated data. With keyCheck
// false the key check row is missing (a 0.1 database). It returns the
// path and the stack IDs.
func legacyDB(t *testing.T, keyCheck bool) (string, []int64) {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(ctx, dbPath, testBox(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	var ids []int64
	for _, name := range []string{"web", "db"} {
		id := stackID(t, s, name)
		if _, err := s.SetRepository(ctx, id, "/repos/"+name, "pw-"+name, map[string]string{"RESTIC_REST_USERNAME": name}); err != nil {
			t.Fatal(err)
		}
		plain := []byte(`{"password":"pw-` + name + `","env":{"RESTIC_REST_USERNAME":"` + name + `"}}`)
		enc, err := s.box.Seal(plain, legacyRepoAAD(id))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, `UPDATE repositories SET secret_enc = ? WHERE stack_id = ?`, enc, id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}

	if _, err := s.db.ExecContext(ctx, `DELETE FROM meta WHERE key = 'key_check'`); err != nil {
		t.Fatal(err)
	}
	if keyCheck {
		sealed, err := s.box.Seal(legacyKeyCheckPlain, legacyKeyCheckAAD)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES ('key_check', ?)`, sealed); err != nil {
			t.Fatal(err)
		}
	}
	return dbPath, ids
}

// assertConverted checks that every secret and the key check open with
// the new associated data only.
func assertConverted(t *testing.T, s *Store, ids []int64) {
	t.Helper()
	ctx := context.Background()
	for i, id := range ids {
		r, err := s.RepositoryForStack(ctx, id)
		if err != nil {
			t.Fatalf("stack %d: %v", id, err)
		}
		name := []string{"web", "db"}[i]
		if r.Password != "pw-"+name || r.Env["RESTIC_REST_USERNAME"] != name {
			t.Errorf("stack %d: password %q, env %v", id, r.Password, r.Env)
		}
		var enc []byte
		if err := s.db.QueryRowContext(ctx, `SELECT secret_enc FROM repositories WHERE stack_id = ?`, id).Scan(&enc); err != nil {
			t.Fatal(err)
		}
		if _, err := s.box.Open(enc, legacyRepoAAD(id)); err == nil {
			t.Errorf("stack %d: secret still opens with the old associated data", id)
		}
	}
	var sealed []byte
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'key_check'`).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if pt, err := s.box.Open(sealed, keyCheckAAD); err != nil || !bytes.Equal(pt, keyCheckPlain) {
		t.Errorf("key check does not open with the new associated data: %v", err)
	}
}

func TestOpenConvertsLegacyEncryption(t *testing.T) {
	tests := []struct {
		name     string
		keyCheck bool
	}{
		{"0.2 to 0.4 database (key check row)", true},
		{"0.1 database (no key check row)", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			dbPath, ids := legacyDB(t, tt.keyCheck)

			s, err := Open(ctx, dbPath, testBox(t))
			if err != nil {
				t.Fatal(err)
			}
			assertConverted(t, s, ids)
			_ = s.Close()

			// The second start finds nothing to convert.
			s, err = Open(ctx, dbPath, testBox(t))
			if err != nil {
				t.Fatal(err)
			}
			assertConverted(t, s, ids)
			_ = s.Close()

			if _, err := Open(ctx, dbPath, boxWithKey(t, otherKey)); !errors.Is(err, ErrKeyMismatch) {
				t.Fatalf("wrong key after conversion: err = %v, want ErrKeyMismatch", err)
			}
		})
	}
}

// A wrong key must not convert or write anything: afterwards the right
// key still finds the old data and converts it.
func TestOpenLegacyWrongKeyChangesNothing(t *testing.T) {
	for _, keyCheck := range []bool{true, false} {
		ctx := context.Background()
		dbPath, ids := legacyDB(t, keyCheck)

		if _, err := Open(ctx, dbPath, boxWithKey(t, otherKey)); !errors.Is(err, ErrKeyMismatch) {
			t.Fatalf("keyCheck=%v: err = %v, want ErrKeyMismatch", keyCheck, err)
		}
		s, err := Open(ctx, dbPath, testBox(t))
		if err != nil {
			t.Fatal(err)
		}
		assertConverted(t, s, ids)
		_ = s.Close()
	}
}

// A secret that was already converted, or that opens with neither
// associated data, is left as it is; the others are converted.
func TestOpenLegacyMixedRows(t *testing.T) {
	ctx := context.Background()
	dbPath, ids := legacyDB(t, true)

	s, err := Open(ctx, dbPath, testBox(t))
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	// Back to a legacy key check, with one secret broken and one converted.
	s2, err := Open(ctx, dbPath, testBox(t))
	if err != nil {
		t.Fatal(err)
	}
	broken := []byte("not a ciphertext of this key, long enough for a nonce")
	if _, err := s2.db.ExecContext(ctx, `UPDATE repositories SET secret_enc = ? WHERE stack_id = ?`, broken, ids[1]); err != nil {
		t.Fatal(err)
	}
	sealed, err := s2.box.Seal(legacyKeyCheckPlain, legacyKeyCheckAAD)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s2.db.ExecContext(ctx, `UPDATE meta SET value = ? WHERE key = 'key_check'`, sealed); err != nil {
		t.Fatal(err)
	}
	_ = s2.Close()

	s, err = Open(ctx, dbPath, testBox(t))
	if err != nil {
		t.Fatalf("a broken secret must not block the start: %v", err)
	}
	defer func() { _ = s.Close() }()
	if r, err := s.RepositoryForStack(ctx, ids[0]); err != nil || r.Password != "pw-web" {
		t.Errorf("converted secret: %+v, %v", r, err)
	}
	if _, err := s.RepositoryForStack(ctx, ids[1]); err == nil {
		t.Error("broken secret decrypted")
	}
	var enc []byte
	if err := s.db.QueryRowContext(ctx, `SELECT secret_enc FROM repositories WHERE stack_id = ?`, ids[1]).Scan(&enc); err != nil || !bytes.Equal(enc, broken) {
		t.Errorf("broken secret was changed: %q, %v", enc, err)
	}
}
