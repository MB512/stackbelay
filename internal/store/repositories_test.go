package store

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/MB512/stackvault/internal/restic"
	"github.com/MB512/stackvault/internal/secrets"
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

func TestRepositoryRoundTripAndEncryption(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	web := stackID(t, s, "web")
	env := map[string]string{"RESTIC_REST_USERNAME": "web", "RESTIC_REST_PASSWORD": "rest-secret-pw"}

	id, err := s.SetRepository(ctx, web, "rest:https://backup.example/web", "repo-secret-pw", env)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.RepositoryForStack(ctx, web)
	if err != nil {
		t.Fatal(err)
	}
	if r.ID != id || r.Location != "rest:https://backup.example/web" || r.Password != "repo-secret-pw" ||
		!reflect.DeepEqual(r.Env, env) || !r.SnapshotsRefreshedAt.IsZero() {
		t.Fatalf("unexpected repository %+v", r)
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

func TestRepositorySecretBoundToStack(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	a, b := stackID(t, s, "a"), stackID(t, s, "b")
	if _, err := s.SetRepository(ctx, a, "/repos/a", "pw-a", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetRepository(ctx, b, "/repos/b", "pw-b", nil); err != nil {
		t.Fatal(err)
	}
	// An attacker with write access to the database copies a's secret to b.
	if _, err := s.db.ExecContext(ctx,
		`UPDATE repositories SET secret_enc = (SELECT secret_enc FROM repositories WHERE stack_id = ?) WHERE stack_id = ?`, a, b); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RepositoryForStack(ctx, b); !errors.Is(err, secrets.ErrDecrypt) {
		t.Fatalf("want ErrDecrypt for moved secret, got %v", err)
	}
}

func TestRepositoryWrongKey(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	web := stackID(t, s, "web")
	if _, err := s.SetRepository(ctx, web, "/repos/web", "pw", nil); err != nil {
		t.Fatal(err)
	}
	other, _ := secrets.New(bytes.Repeat([]byte{7}, secrets.KeySize))
	s.box = other
	if _, err := s.RepositoryForStack(ctx, web); !errors.Is(err, secrets.ErrDecrypt) {
		t.Fatalf("want ErrDecrypt, got %v", err)
	}
}

func TestSetRepositoryLocationChangeClearsCache(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	web := stackID(t, s, "web")
	id, err := s.SetRepository(ctx, web, "/repos/web", "pw", nil)
	if err != nil {
		t.Fatal(err)
	}
	snap := restic.Snapshot{ID: "0123456789abcdef", ShortID: "01234567", Time: time.Now(), Hostname: "h", Paths: []string{"/s/web"}}
	if err := s.ReplaceSnapshots(ctx, id, []restic.Snapshot{snap}, time.Now()); err != nil {
		t.Fatal(err)
	}

	// Same location, new password: cache stays, id stays.
	id2, err := s.SetRepository(ctx, web, "/repos/web", "new-pw", nil)
	if err != nil || id2 != id {
		t.Fatalf("id %d -> %d, err %v", id, id2, err)
	}
	if snaps, _ := s.ListSnapshots(ctx, id); len(snaps) != 1 {
		t.Fatalf("cache dropped on password change: %v", snaps)
	}

	// New location: cache dropped.
	if _, err := s.SetRepository(ctx, web, "/repos/web-new", "new-pw", nil); err != nil {
		t.Fatal(err)
	}
	r, _ := s.RepositoryForStack(ctx, web)
	snaps, _ := s.ListSnapshots(ctx, id)
	if len(snaps) != 0 || !r.SnapshotsRefreshedAt.IsZero() || r.Password != "new-pw" {
		t.Fatalf("after location change: snaps=%v repo=%+v", snaps, r)
	}
}

func TestSetRepositoryValidation(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	web := stackID(t, s, "web")

	tests := []struct {
		name     string
		stack    int64
		location string
		password string
		env      map[string]string
		wantErr  string
	}{
		{"unknown stack", 999, "/r", "pw", nil, "not found"},
		{"empty location", web, "", "pw", nil, "location"},
		{"spaces", web, " /r", "pw", nil, "location"},
		{"newline", web, "/r\n--x", "pw", nil, "control"},
		{"flag", web, "--insecure-tls", "pw", nil, "'-'"},
		{"password in rest url", web, "rest:https://u:secret@host/r", "pw", nil, "password"},
		{"password in s3 url", web, "s3:https://ak:sk@s3.example/b", "pw", nil, "password"},
		{"empty password", web, "/r", "", nil, "password"},
		{"password command", web, "/r", "pw", map[string]string{"RESTIC_PASSWORD_COMMAND": "id"}, "not allowed"},
		{"password in env", web, "/r", "pw", map[string]string{"RESTIC_PASSWORD": "x"}, "not allowed"},
		{"arbitrary env", web, "/r", "pw", map[string]string{"LD_PRELOAD": "/x.so"}, "not allowed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := s.SetRepository(ctx, tt.stack, tt.location, tt.password, tt.env)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
			}
		})
	}

	// Username without password in the URL is fine.
	if _, err := s.SetRepository(ctx, web, "rest:https://user@host/r", "pw", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RepositoryForStack(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}
