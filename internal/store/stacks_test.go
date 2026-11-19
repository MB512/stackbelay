package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSyncStacks(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	steps := []struct {
		discovered []string
		want       map[string]bool // name -> present
	}{
		{[]string{"web", "db"}, map[string]bool{"db": true, "web": true}},
		{[]string{"web"}, map[string]bool{"db": false, "web": true}},                          // db vanished: kept, not present
		{[]string{"web", "db", "new"}, map[string]bool{"db": true, "new": true, "web": true}}, // db is back
		{nil, map[string]bool{"db": false, "new": false, "web": false}},
	}
	ids := map[string]int64{}
	for i, step := range steps {
		if err := s.SyncStacks(ctx, step.discovered); err != nil {
			t.Fatal(err)
		}
		list, err := s.ListStacks(ctx)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]bool{}
		for _, st := range list {
			got[st.Name] = st.Present
			if id, ok := ids[st.Name]; ok && id != st.ID {
				t.Errorf("step %d: id of %s changed from %d to %d", i, st.Name, id, st.ID)
			}
			ids[st.Name] = st.ID
		}
		if !reflect.DeepEqual(got, step.want) {
			t.Fatalf("step %d: got %v, want %v", i, got, step.want)
		}
	}
}

func TestStackByName(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if err := s.SyncStacks(ctx, []string{"web"}); err != nil {
		t.Fatal(err)
	}
	if st, err := s.StackByName(ctx, "web"); err != nil || st.Name != "web" || !st.Present {
		t.Fatalf("StackByName = %+v, %v", st, err)
	}
	if _, err := s.StackByName(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// Migration 0016 (issue #37) drops the stack-level enabled flag: the
// schedules of a disabled stack are disabled, the others are unchanged.
func TestScheduleEnabledMigration(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	raw, err := sql.Open("sqlite", dbPath+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
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
		if m.version > 15 {
			break
		}
		exec(m.sql)
		exec(`INSERT INTO schema_migrations VALUES (?, ?, '2026-11-01T00:00:00Z')`, m.version, m.name)
	}
	exec(`INSERT INTO stacks (id, name, enabled) VALUES (1, 'on', 1), (2, 'off', 0)`)
	exec(`INSERT INTO schedules (id, stack_id, cron_expr, job_type, enabled) VALUES
	      (1, 1, '@daily', 'backup', 1), (2, 1, '@weekly', 'update', 0), (3, 2, '@daily', 'backup', 1)`)
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, dbPath, testBox(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for id, want := range map[int64]bool{1: true, 2: false, 3: false} {
		if sc, err := s.ScheduleByID(ctx, id); err != nil || sc.Enabled != want {
			t.Errorf("schedule %d: %+v, %v; want enabled=%v", id, sc, err, want)
		}
	}
}

// SeenPresent (issue #27): false only for an imported stack that was never
// on disk; once present, it stays true when the directory vanishes.
func TestStackSeenPresent(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if err := s.SyncStacks(ctx, []string{"web"}); err != nil {
		t.Fatal(err)
	}
	cloud, err := s.CreateImportedStack(ctx, "cloud", "old-host")
	if err != nil || cloud.SeenPresent || cloud.Present {
		t.Fatalf("imported: %+v, %v", cloud, err)
	}
	if err := s.SetStackPresent(ctx, cloud.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncStacks(ctx, nil); err != nil { // both vanish
		t.Fatal(err)
	}
	for _, name := range []string{"web", "cloud"} {
		if st, _ := s.StackByName(ctx, name); st.Present || !st.SeenPresent {
			t.Errorf("%s after vanishing: %+v", name, st)
		}
	}
}

// Migration 0017 marks existing records as seen, except imported stacks
// that are not present and were never restored successfully.
func TestSeenPresentMigration(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	raw, err := sql.Open("sqlite", dbPath+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
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
		if m.version > 16 {
			break
		}
		exec(m.sql)
		exec(`INSERT INTO schema_migrations VALUES (?, ?, '2026-11-01T00:00:00Z')`, m.version, m.name)
	}
	exec(`INSERT INTO stacks (id, name, present, source_host) VALUES
	      (1, 'here', 1, ''), (2, 'vanished', 0, ''), (3, 'imported', 0, 'old-host'), (4, 'moved-and-gone', 0, 'old-host')`)
	exec(`INSERT INTO jobs (id, stack_id, type, trigger, state, options, queued_at, finished_at, result)
	      VALUES ('01AAAAAAAAAAAAAAAAAAAAAAA1', 4, 'restore', 'manual', 'succeeded', '{}', '2026-11-01T10:00:00Z', '2026-11-01T11:00:00Z', '{}')`)
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, dbPath, testBox(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for name, want := range map[string]bool{"here": true, "vanished": true, "imported": false, "moved-and-gone": true} {
		if st, err := s.StackByName(ctx, name); err != nil || st.SeenPresent != want {
			t.Errorf("%s: %+v, %v; want seen_present=%v", name, st, err, want)
		}
	}
}
