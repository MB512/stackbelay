package store

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MB512/stackbelay/internal/restic"
	"github.com/MB512/stackbelay/internal/secrets"
)

func stackID(t *testing.T, s *Store, name string) int64 {
	t.Helper()
	ctx := context.Background()
	stacks, err := s.ListStacks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{name}
	for _, st := range stacks {
		names = append(names, st.Name)
	}
	if err := s.SyncStacks(ctx, names); err != nil {
		t.Fatal(err)
	}
	st, err := s.StackByName(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	return st.ID
}

// setRepo gives a stack the repository named after location, creating it
// if no repository has that name yet (stacks with the same location share
// it). It returns the repository id.
func setRepo(ctx context.Context, s *Store, stackID int64, location, password string, env map[string]string) (int64, error) {
	repos, err := s.ListRepositories(ctx)
	if err != nil {
		return 0, err
	}
	var id int64
	for _, r := range repos {
		if r.Name == location {
			id = r.ID
		}
	}
	if id == 0 {
		if id, err = s.CreateRepository(ctx, location, location, password, env); err != nil {
			return 0, err
		}
	}
	return id, s.SetStackRepository(ctx, stackID, id)
}

func TestRepositoryRoundTripAndEncryption(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	web := stackID(t, s, "web")
	env := map[string]string{"RESTIC_REST_USERNAME": "web", "RESTIC_REST_PASSWORD": "rest-secret-pw"}

	id, err := s.CreateRepository(ctx, "nas", "rest:https://backup.example/web", "repo-secret-pw", env)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RepositoryForStack(ctx, web); !errors.Is(err, ErrNotFound) {
		t.Fatalf("stack without repository: %v", err)
	}
	if err := s.SetStackRepository(ctx, web, id); err != nil {
		t.Fatal(err)
	}
	r, err := s.RepositoryForStack(ctx, web)
	if err != nil {
		t.Fatal(err)
	}
	if r.ID != id || r.Name != "nas" || r.Location != "rest:https://backup.example/web" || r.Password != "repo-secret-pw" ||
		!reflect.DeepEqual(r.Env, env) || !r.SnapshotsRefreshedAt.IsZero() {
		t.Fatalf("unexpected repository %+v", r)
	}
	if byID, err := s.Repository(ctx, id); err != nil || !reflect.DeepEqual(byID, r) {
		t.Fatalf("Repository(%d) = %+v, %v", id, byID, err)
	}
	want := restic.Repo{Location: r.Location, Env: []string{
		"RESTIC_PASSWORD=repo-secret-pw", "RESTIC_REST_PASSWORD=rest-secret-pw", "RESTIC_REST_USERNAME=web",
	}}
	if got := r.Restic(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Restic() = %+v, want %+v", got, want)
	}

	// The secrets are not stored in plain text.
	var raw []byte
	if err := s.db.QueryRowContext(ctx, `SELECT secret_enc FROM repositories WHERE id = ?`, id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"repo-secret-pw", "rest-secret-pw"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("%q stored in plain text", secret)
		}
	}
}

func TestRepositorySecretBoundToRow(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	a, err := s.CreateRepository(ctx, "a", "/repos/a", "pw-a", nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.CreateRepository(ctx, "b", "/repos/b", "pw-b", nil)
	if err != nil {
		t.Fatal(err)
	}
	// An attacker with write access to the database copies a's secret to b.
	if _, err := s.db.ExecContext(ctx,
		`UPDATE repositories SET secret_enc = (SELECT secret_enc FROM repositories WHERE id = ?) WHERE id = ?`, a, b); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Repository(ctx, b); !errors.Is(err, secrets.ErrDecrypt) {
		t.Fatalf("want ErrDecrypt for moved secret, got %v", err)
	}
}

func TestRepositoryWrongKey(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	web := stackID(t, s, "web")
	if _, err := setRepo(ctx, s, web, "/repos/web", "pw", nil); err != nil {
		t.Fatal(err)
	}
	other, _ := secrets.New(bytes.Repeat([]byte{7}, secrets.KeySize))
	s.box = other
	if _, err := s.RepositoryForStack(ctx, web); !errors.Is(err, secrets.ErrDecrypt) {
		t.Fatalf("want ErrDecrypt, got %v", err)
	}
}

func TestUpdateRepository(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	id, err := s.CreateRepository(ctx, "nas", "/repos/web", "pw", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRepository(ctx, "other", "/repos/other", "pw", nil); err != nil {
		t.Fatal(err)
	}
	snap := restic.Snapshot{ID: "0123456789abcdef", ShortID: "01234567", Time: time.Now(), Hostname: "h", Paths: []string{"/s/web"}}
	if err := s.ReplaceSnapshots(ctx, id, []restic.Snapshot{snap}, time.Now()); err != nil {
		t.Fatal(err)
	}

	// Same location, new name and password: cache stays.
	if err := s.UpdateRepository(ctx, id, "nas-2026", "/repos/web", "new-pw", nil); err != nil {
		t.Fatal(err)
	}
	if snaps, _ := s.ListSnapshots(ctx, id); len(snaps) != 1 {
		t.Fatalf("cache dropped on password change: %v", snaps)
	}

	// New location: cache dropped.
	if err := s.UpdateRepository(ctx, id, "nas-2026", "/repos/web-new", "new-pw", nil); err != nil {
		t.Fatal(err)
	}
	r, _ := s.Repository(ctx, id)
	snaps, _ := s.ListSnapshots(ctx, id)
	if len(snaps) != 0 || !r.SnapshotsRefreshedAt.IsZero() || r.Password != "new-pw" || r.Name != "nas-2026" {
		t.Fatalf("after location change: snaps=%v repo=%+v", snaps, r)
	}

	if err := s.UpdateRepository(ctx, id, "other", "/repos/web-new", "pw", nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("name of another repository: %v", err)
	}
	if err := s.UpdateRepository(ctx, 999, "x", "/r", "pw", nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown repository: %v", err)
	}
}

// Several stacks share a repository; it can be deleted only when no stack
// uses it, and its snapshot cache goes with it.
func TestSharedRepository(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	web, db, wiki := stackID(t, s, "web"), stackID(t, s, "db"), stackID(t, s, "wiki")
	id, err := s.CreateRepository(ctx, "nas", "/repos/shared", "pw", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRepository(ctx, "nas", "/repos/x", "pw", nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate name: %v", err)
	}
	for _, st := range []int64{web, db} {
		if err := s.SetStackRepository(ctx, st, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.UpdateRepository(ctx, id, "nas", "/repos/shared", "new-pw", nil); err != nil {
		t.Fatal(err)
	}
	for _, st := range []int64{web, db} {
		if r, err := s.RepositoryForStack(ctx, st); err != nil || r.ID != id || r.Password != "new-pw" {
			t.Fatalf("stack %d: %+v, %v", st, r, err)
		}
	}
	if _, err := s.RepositoryInfoForStack(ctx, wiki); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wiki: %v", err)
	}

	list, err := s.ListRepositories(ctx)
	if err != nil || len(list) != 1 || !slices.Equal(list[0].Stacks, []string{"db", "web"}) {
		t.Fatalf("ListRepositories = %+v, %v", list, err)
	}
	if ri, err := s.RepositoryInfo(ctx, id); err != nil || ri.Name != "nas" || len(ri.Stacks) != 2 {
		t.Fatalf("RepositoryInfo = %+v, %v", ri, err)
	}
	sums, err := s.StackSummaries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, ss := range sums {
		if want := ss.Name != "wiki"; ss.HasRepository != want || (want && ss.RepositoryName != "nas") {
			t.Errorf("summary %s: %+v", ss.Name, ss)
		}
	}

	snap := restic.Snapshot{ID: "0123456789abcdef", ShortID: "01234567", Time: time.Now(), Hostname: "h", Paths: []string{"/s/web"}}
	if err := s.ReplaceSnapshots(ctx, id, []restic.Snapshot{snap}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteRepository(ctx, id); !errors.Is(err, ErrInUse) {
		t.Fatalf("delete while in use: %v", err)
	}
	for _, st := range []int64{web, db} {
		if err := s.SetStackRepository(ctx, st, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeleteRepository(ctx, id); err != nil {
		t.Fatal(err)
	}
	if snaps, _ := s.ListSnapshots(ctx, id); len(snaps) != 0 {
		t.Fatalf("snapshot cache left: %v", snaps)
	}
	if err := s.DeleteRepository(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
	if err := s.SetStackRepository(ctx, web, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted repository: %v", err)
	}
	if err := s.SetStackRepository(ctx, 999, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown stack: %v", err)
	}
}

func TestCreateRepositoryValidation(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	tests := []struct {
		name     string
		repo     string
		location string
		password string
		env      map[string]string
		wantErr  string
	}{
		{"empty name", "", "/r", "pw", nil, "name"},
		{"name with spaces", " nas", "/r", "pw", nil, "name"},
		{"name with newline", "na\ns", "/r", "pw", nil, "name"},
		{"long name", strings.Repeat("n", MaxRepositoryNameLen+1), "/r", "pw", nil, "longer"},
		{"empty location", "n", "", "pw", nil, "location"},
		{"spaces", "n", " /r", "pw", nil, "location"},
		{"newline", "n", "/r\n--x", "pw", nil, "control"},
		{"flag", "n", "--insecure-tls", "pw", nil, "'-'"},
		{"password in rest url", "n", "rest:https://u:secret@host/r", "pw", nil, "password"},
		{"password in s3 url", "n", "s3:https://ak:sk@s3.example/b", "pw", nil, "password"},
		{"password with slash", "n", "rest:https://u:12/x@host/r", "pw", nil, "cannot be parsed"},
		{"password with hash", "n", "rest:https://u:p#w@host/r", "pw", nil, "cannot be parsed"},
		{"empty password", "n", "/r", "", nil, "password"},
		{"password command", "n", "/r", "pw", map[string]string{"RESTIC_PASSWORD_COMMAND": "id"}, "not allowed"},
		{"password in env", "n", "/r", "pw", map[string]string{"RESTIC_PASSWORD": "x"}, "not allowed"},
		{"arbitrary env", "n", "/r", "pw", map[string]string{"LD_PRELOAD": "/x.so"}, "not allowed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := s.CreateRepository(ctx, tt.repo, tt.location, tt.password, tt.env)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
			}
		})
	}

	// Username without password in the URL is fine.
	if _, err := s.CreateRepository(ctx, "n", "rest:https://user@host/r", "pw", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RepositoryForStack(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}
