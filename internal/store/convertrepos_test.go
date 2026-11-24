package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
)

// oldDB creates a database as migration 0017 left it, with the per-stack
// repositories of rows (stack name -> location, password; sealed with the
// old associated data) and one cached snapshot per repository. If keyCheck
// is false the key_check row is missing.
func oldDB(t *testing.T, rows []oldRepo, keyCheck bool) string {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	raw, err := sql.Open("sqlite", dbPath+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	ms, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := raw.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at TEXT NOT NULL) STRICT`)
	for _, m := range ms {
		if m.version > 17 {
			break
		}
		exec(m.sql)
		exec(`INSERT INTO schema_migrations VALUES (?, ?, '2026-11-01T00:00:00Z')`, m.version, m.name)
	}
	box := testBox(t)
	for i, r := range rows {
		stackID := int64(i + 1)
		exec(`INSERT INTO stacks (id, name) VALUES (?, ?)`, stackID, r.stack)
		plain, _ := json.Marshal(repoSecret{Password: r.password, Env: r.env})
		enc, err := box.Seal(plain, ownerAAD(stackID))
		if err != nil {
			t.Fatal(err)
		}
		if r.broken {
			enc[len(enc)-1] ^= 1
		}
		exec(`INSERT INTO repositories (id, stack_id, location, secret_enc) VALUES (?, ?, ?, ?)`, 10+stackID, stackID, r.location, enc)
		exec(`INSERT INTO snapshots (repo_id, snapshot_id, short_id, time, hostname, paths)
		      VALUES (?, 'snap', 'snap', '2026-11-01T00:00:00Z', 'h', '["/s/x"]')`, 10+stackID)
	}
	if keyCheck {
		sealed, err := box.Seal(keyCheckPlain, keyCheckAAD)
		if err != nil {
			t.Fatal(err)
		}
		exec(`INSERT INTO meta (key, value) VALUES ('key_check', ?)`, sealed)
	}
	return dbPath
}

type oldRepo struct {
	stack, location, password string
	env                       map[string]string
	broken                    bool // secret does not decrypt
}

func TestConvertRepositories(t *testing.T) {
	for _, keyCheck := range []bool{true, false} {
		t.Run(map[bool]string{true: "with key check", false: "without key check"}[keyCheck], func(t *testing.T) {
			ctx := context.Background()
			env := map[string]string{"RESTIC_REST_USERNAME": "u"}
			dbPath := oldDB(t, []oldRepo{
				{stack: "a", location: "/repos/shared", password: "pw", env: env},  // repo 11
				{stack: "b", location: "/repos/shared", password: "pw", env: env},  // 12: merged into 11
				{stack: "c", location: "/repos/shared", password: "other"},         // 13: same location, other password
				{stack: "d", location: "/repos/d", password: "pw-d"},               // 14
				{stack: "e", location: "/repos/e", password: "pw-e", broken: true}, // 15: left alone
			}, keyCheck)
			s, err := Open(ctx, dbPath, testBox(t))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()

			want := map[string]struct {
				id       int64
				name     string
				password string
			}{
				"a": {11, "/repos/shared", "pw"},
				"b": {11, "/repos/shared", "pw"},
				"c": {13, "/repos/shared (2)", "other"},
				"d": {14, "/repos/d", "pw-d"},
			}
			for name, w := range want {
				r, err := s.RepositoryForStack(ctx, stackID(t, s, name))
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				if r.ID != w.id || r.Name != w.name || r.Password != w.password {
					t.Errorf("%s: %+v, want %+v", name, r, w)
				}
			}
			if r, _ := s.RepositoryForStack(ctx, stackID(t, s, "a")); !reflect.DeepEqual(r.Env, env) {
				t.Errorf("env %v", r.Env)
			}
			// The broken secret keeps its placeholder name and stays linked.
			ri, err := s.RepositoryInfoForStack(ctx, stackID(t, s, "e"))
			if err != nil || ri.ID != 15 || ri.Name != "repository-15" {
				t.Errorf("e: %+v, %v", ri, err)
			}
			if _, err := s.Repository(ctx, 12); !errors.Is(err, ErrNotFound) {
				t.Errorf("merged repository 12 still exists: %v", err)
			}
			// The snapshot cache survived the rebuild of the table.
			if snaps, err := s.ListSnapshots(ctx, 14); err != nil || len(snaps) != 1 {
				t.Errorf("snapshot cache of 14: %v, %v", snaps, err)
			}
			if pending, _ := s.conversionPending(ctx); pending {
				t.Error("conversion still pending")
			}
			_ = s.Close()

			// A second start finds nothing to convert; a wrong key is refused.
			s2, err := Open(ctx, dbPath, testBox(t))
			if err != nil {
				t.Fatal(err)
			}
			_ = s2.Close()
			if _, err := Open(ctx, dbPath, boxWithKey(t, otherKey)); !errors.Is(err, ErrKeyMismatch) {
				t.Fatalf("wrong key: %v", err)
			}
		})
	}
}

// A wrong key is refused before anything is converted.
func TestConvertRepositoriesWrongKey(t *testing.T) {
	ctx := context.Background()
	for _, keyCheck := range []bool{true, false} {
		dbPath := oldDB(t, []oldRepo{{stack: "a", location: "/repos/a", password: "pw"}}, keyCheck)
		if _, err := Open(ctx, dbPath, boxWithKey(t, otherKey)); !errors.Is(err, ErrKeyMismatch) {
			t.Fatalf("key check %t: err = %v, want ErrKeyMismatch", keyCheck, err)
		}
		s, err := Open(ctx, dbPath, testBox(t))
		if err != nil {
			t.Fatal(err)
		}
		if r, err := s.RepositoryForStack(ctx, stackID(t, s, "a")); err != nil || r.Password != "pw" {
			t.Fatalf("key check %t: %+v, %v", keyCheck, r, err)
		}
		_ = s.Close()
	}
}
