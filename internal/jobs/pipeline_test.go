package jobs

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/MB512/stackvault/internal/docker"
	"github.com/MB512/stackvault/internal/restic"
	"github.com/MB512/stackvault/internal/stacks"
)

var fixedNow = time.Date(2026, 11, 13, 10, 0, 0, 0, time.UTC)

type env struct {
	calls  *calls
	docker *fakeDocker
	restic *fakeRestic
	p      *Pipeline
	stack  stacks.Stack
}

func newEnv(t *testing.T) *env {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "web")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cf := filepath.Join(dir, "compose.yml")
	if err := os.WriteFile(cf, []byte("services:\n  app: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "data.txt"), []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := &calls{}
	d := &fakeDocker{calls: c, failOn: map[string]error{}, services: []docker.ServiceState{
		{Name: "app", State: "running", ImageRef: "nextcloud:29", ContainerID: "9f8e9f8e9f8e",
			ImageID: testImageID, RepoDigests: []string{"other@sha256:" + strings.Repeat("0", 64), testDigest}},
		{Name: "db", State: "running", ImageRef: "postgres:16", ContainerID: "db0db0db0db0"},
		{Name: "tool", State: "exited", ContainerID: "7007700770077"},
		{Name: "never"}, // defined, but no container (e.g. added while the stack was stopped)
	}}
	r := &fakeRestic{calls: c, failOn: map[string]error{}}
	return &env{
		calls:  c,
		docker: d,
		restic: r,
		stack:  stacks.Stack{Name: "web", Dir: dir, ComposeFile: cf},
		p: &Pipeline{
			Docker: d, Restic: r, Host: "host-a",
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
			Now:    func() time.Time { return fixedNow },
		},
	}
}

func assertCalls(t *testing.T, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("call sequence mismatch\ngot:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

func TestBackup(t *testing.T) {
	const (
		check    = "restic.CheckRepo"
		services = "docker.Services"
		prestop  = "restic.Backup tag=pre-stop parent= host=host-a"
		stop     = "docker.Stop"
		start    = "docker.Start app,db"
		healthy  = "docker.WaitHealthy"
	)
	tests := []struct {
		name      string
		opts      BackupOptions
		setup     func(e *env)
		wantCalls []string
		wantErr   bool
		wantRes   BackupResult
	}{
		{
			name:      "with pre-stop snapshot",
			opts:      BackupOptions{PreStop: true},
			wantCalls: []string{check, services, prestop, stop, "restic.Backup tag=final parent=snap-pre-stop host=host-a", start, healthy},
			wantRes:   BackupResult{PreStopSnapshot: "snap-pre-stop", Snapshot: "snap-final"},
		},
		{
			name:      "without pre-stop snapshot",
			wantCalls: []string{check, services, stop, "restic.Backup tag=final parent= host=host-a", start, healthy},
			wantRes:   BackupResult{Snapshot: "snap-final"},
		},
		{
			name:      "pre-stop snapshot fails: continue without parent",
			opts:      BackupOptions{PreStop: true},
			setup:     func(e *env) { e.restic.failOn["Backup:pre-stop"] = errBoom },
			wantCalls: []string{check, services, prestop, stop, "restic.Backup tag=final parent= host=host-a", start, healthy},
			wantRes:   BackupResult{Snapshot: "snap-final"},
		},
		{
			name:      "final snapshot fails after stop: stack still restarts",
			setup:     func(e *env) { e.restic.failOn["Backup:final"] = errBoom },
			wantCalls: []string{check, services, stop, "restic.Backup tag=final parent= host=host-a", start, healthy},
			wantErr:   true,
		},
		{
			name:      "stop fails: stack still restarts",
			setup:     func(e *env) { e.docker.failOn["Stop"] = errBoom },
			wantCalls: []string{check, services, stop, start, healthy},
			wantErr:   true,
		},
		{
			name:      "start fails: job fails",
			setup:     func(e *env) { e.docker.failOn["Start"] = errBoom },
			wantCalls: []string{check, services, stop, "restic.Backup tag=final parent= host=host-a", start},
			wantErr:   true,
			wantRes:   BackupResult{Snapshot: "snap-final"},
		},
		{
			name:      "unhealthy after start: job still succeeds",
			setup:     func(e *env) { e.docker.failOn["WaitHealthy"] = errBoom },
			wantCalls: []string{check, services, stop, "restic.Backup tag=final parent= host=host-a", start, healthy},
			wantRes:   BackupResult{Snapshot: "snap-final"},
		},
		{
			name:      "repository unreachable: nothing is stopped",
			setup:     func(e *env) { e.restic.failOn["CheckRepo"] = errBoom },
			wantCalls: []string{check},
			wantErr:   true,
		},
		{
			name:      "nothing running: stays stopped",
			setup:     func(e *env) { e.docker.services = []docker.ServiceState{{Name: "app", State: "exited"}} },
			wantCalls: []string{check, services, stop, "restic.Backup tag=final parent= host=host-a"},
			wantRes:   BackupResult{Snapshot: "snap-final"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			if tt.setup != nil {
				tt.setup(e)
			}
			res, err := e.p.Backup(context.Background(), e.stack, tt.opts)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && !errors.Is(err, errBoom) {
				t.Errorf("error should wrap the cause, got %v", err)
			}
			if res != tt.wantRes {
				t.Errorf("result = %+v, want %+v", res, tt.wantRes)
			}
			assertCalls(t, e.calls.list(), tt.wantCalls)
		})
	}
}

func TestBackupCancelledStillRestarts(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.restic.cancelOnFinal = cancel

	_, err := e.p.Backup(ctx, e.stack, BackupOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	calls := e.calls.list()
	if calls[len(calls)-2] != "docker.Start app,db" {
		t.Fatalf("stack was not restarted: %v", calls)
	}
	if e.docker.startCtxErr != nil {
		t.Fatalf("restart ran with a cancelled context: %v", e.docker.startCtxErr)
	}
}

func TestBackupWritesMetadata(t *testing.T) {
	e := newEnv(t)
	if _, err := e.p.Backup(context.Background(), e.stack, BackupOptions{}); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Join(e.stack.Dir, MetadataFile))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	m, err := ParseMetadata(f, "web")
	if err != nil {
		t.Fatal(err)
	}
	if !m.CreatedAt.Equal(fixedNow) || len(m.ComposeSHA256) != 64 {
		t.Errorf("unexpected header %+v", m)
	}
	want := []ServiceRecord{
		{Name: "app", State: "running", ImageRef: "nextcloud:29", RepoDigest: testDigest, ImageID: testImageID, ContainerID: "9f8e9f8e9f8e"},
		{Name: "db", State: "running", ImageRef: "postgres:16", ContainerID: "db0db0db0db0"},
		{Name: "tool", State: "exited", ContainerID: "7007700770077"},
		{Name: "never", State: "-"},
	}
	if !reflect.DeepEqual(m.Services, want) {
		t.Errorf("services:\ngot  %+v\nwant %+v", m.Services, want)
	}
	if _, err := os.Stat(filepath.Join(e.stack.Dir, MetadataFile+".tmp")); !os.IsNotExist(err) {
		t.Error("temporary metadata file left behind")
	}
}

func TestPickRepoDigest(t *testing.T) {
	h := strings.Repeat("a", 64)
	tests := []struct {
		ref     string
		digests []string
		want    string
	}{
		{"nextcloud:29", []string{"foo@sha256:" + h, "nextcloud@sha256:" + h}, "nextcloud@sha256:" + h},
		{"ghcr.io/a/b:1.2", []string{"ghcr.io/a/b@sha256:" + h}, "ghcr.io/a/b@sha256:" + h},
		{"docker.io/library/redis", []string{"redis@sha256:" + h}, "redis@sha256:" + h}, // fallback
		{"localhost:5000/x:1", []string{"localhost:5000/x@sha256:" + h}, ""},            // ':' not allowed
		{"built", nil, ""},
	}
	for _, tt := range tests {
		if got := pickRepoDigest(tt.ref, tt.digests); got != tt.want {
			t.Errorf("pickRepoDigest(%q) = %q, want %q", tt.ref, got, tt.want)
		}
	}
}

// --- restore ---

const snapID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func restoreEnv(t *testing.T) *env {
	e := newEnv(t)
	e.restic.snapshots = []restic.Snapshot{{
		ID: snapID, ShortID: snapID[:8], Hostname: "host-a",
		Paths: []string{"/home/docker/web"}, Tags: []string{TagFinal},
	}}
	e.restic.restoreFiles = map[string]string{
		"compose.yml": "services:\n  app: {}\n",
		"data.txt":    "restored",
		MetadataFile:  "# stackvault-metadata v1\ncreated_at\t2026-11-01T00:00:00Z\nstack\tweb\napp\trunning\t-\t-\t-\t-\ndb\texited\t-\t-\t-\t-\n",
	}
	e.restic.node = restic.Node{
		Type: "dir", Mode: fs.ModeDir | 0o750,
		UID: uint32(os.Getuid()), GID: uint32(os.Getgid()), //nolint:gosec // G115: ids fit in uint32
		ModTime: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
	return e
}

func readData(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "data.txt"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func assertNoStaging(t *testing.T, e *env) {
	t.Helper()
	if _, err := os.Stat(e.stack.Dir + ".restore-tmp"); !os.IsNotExist(err) {
		t.Fatalf("staging directory left behind: %v", err)
	}
}

func TestRestoreSuccess(t *testing.T) {
	e := restoreEnv(t)
	res, err := e.p.Restore(context.Background(), e.stack, RestoreOptions{SnapshotID: snapID[:8]})
	if err != nil {
		t.Fatal(err)
	}
	assertCalls(t, e.calls.list(), []string{
		"restic.CheckRepo",
		"restic.Snapshots " + snapID[:8],
		"docker.Services",
		"docker.Stop",
		"restic.Backup tag=pre-restore parent= host=host-a",
		"restic.Restore " + snapID + ":/home/docker/web",
		"restic.Node " + snapID + ":/home/docker/web",
		"docker.Up app", // only services running at backup time
		"docker.WaitHealthy",
	})
	wantOld := e.stack.Dir + ".old-20261113T100000Z"
	if res.OldDir != wantOld || res.SafetySnapshot != "snap-pre-restore" || res.Snapshot != snapID {
		t.Errorf("unexpected result %+v", res)
	}
	if got := readData(t, e.stack.Dir); got != "restored" {
		t.Errorf("stack dir has %q", got)
	}
	fi, err := os.Stat(e.stack.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o750 || !fi.ModTime().Equal(e.restic.node.ModTime) {
		t.Errorf("stack dir metadata not restored: mode %v, mtime %v", fi.Mode(), fi.ModTime())
	}
	if got := readData(t, wantOld); got != "original" {
		t.Errorf("old dir has %q", got)
	}
	assertNoStaging(t, e)
}

func TestRestoreRollback(t *testing.T) {
	tests := []struct {
		name      string
		setup     func(e *env)
		wantCalls []string
	}{
		{
			name:  "restic restore fails",
			setup: func(e *env) { e.restic.failOn["Restore"] = errBoom },
			wantCalls: []string{"restic.CheckRepo", "restic.Snapshots " + snapID[:8], "docker.Services", "docker.Stop",
				"restic.Backup tag=pre-restore parent= host=host-a", "restic.Restore " + snapID + ":/home/docker/web",
				"docker.Start app,db", "docker.WaitHealthy"},
		},
		{
			name:  "reading directory metadata fails",
			setup: func(e *env) { e.restic.failOn["Node"] = errBoom },
			wantCalls: []string{"restic.CheckRepo", "restic.Snapshots " + snapID[:8], "docker.Services", "docker.Stop",
				"restic.Backup tag=pre-restore parent= host=host-a", "restic.Restore " + snapID + ":/home/docker/web",
				"restic.Node " + snapID + ":/home/docker/web", "docker.Start app,db", "docker.WaitHealthy"},
		},
		{
			name:  "safety snapshot fails",
			setup: func(e *env) { e.restic.failOn["Backup:pre-restore"] = errBoom },
			wantCalls: []string{"restic.CheckRepo", "restic.Snapshots " + snapID[:8], "docker.Services", "docker.Stop",
				"restic.Backup tag=pre-restore parent= host=host-a", "docker.Start app,db", "docker.WaitHealthy"},
		},
		{
			name:  "stop fails",
			setup: func(e *env) { e.docker.failOn["Stop"] = errBoom },
			wantCalls: []string{"restic.CheckRepo", "restic.Snapshots " + snapID[:8], "docker.Services", "docker.Stop",
				"docker.Start app,db", "docker.WaitHealthy"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := restoreEnv(t)
			tt.setup(e)
			_, err := e.p.Restore(context.Background(), e.stack, RestoreOptions{SnapshotID: snapID[:8]})
			if !errors.Is(err, errBoom) {
				t.Fatalf("expected errBoom, got %v", err)
			}
			assertCalls(t, e.calls.list(), tt.wantCalls)
			if got := readData(t, e.stack.Dir); got != "original" {
				t.Errorf("original stack directory was modified: %q", got)
			}
			assertNoStaging(t, e)
		})
	}
}

func TestRestoreSnapshotWithoutComposeFileRollsBack(t *testing.T) {
	e := restoreEnv(t)
	delete(e.restic.restoreFiles, "compose.yml")
	if _, err := e.p.Restore(context.Background(), e.stack, RestoreOptions{SnapshotID: snapID[:8]}); err == nil {
		t.Fatal("expected error")
	}
	if got := readData(t, e.stack.Dir); got != "original" {
		t.Errorf("original stack directory was modified: %q", got)
	}
	assertNoStaging(t, e)
}

func TestRestoreRejectedBeforeStop(t *testing.T) {
	tests := []struct {
		name  string
		id    string
		setup func(e *env)
	}{
		{"invalid id", "latest", nil},
		{"id with path", snapID[:8] + ":/etc", nil},
		{"not found", snapID[:8], func(e *env) { e.restic.snapshots = nil }},
		{"other stack", snapID[:8], func(e *env) { e.restic.snapshots[0].Paths = []string{"/home/docker/other"} }},
		{"relative path", snapID[:8], func(e *env) { e.restic.snapshots[0].Paths = []string{"web"} }},
		{"unclean path", snapID[:8], func(e *env) { e.restic.snapshots[0].Paths = []string{"/home/docker/../web"} }},
		{"multiple paths", snapID[:8], func(e *env) { e.restic.snapshots[0].Paths = []string{"/a/web", "/b/web"} }},
		{"id mismatch", "fedcba98", nil},
		{"staging exists", snapID[:8], func(e *env) {
			if err := os.Mkdir(e.stack.Dir+".restore-tmp", 0o755); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := restoreEnv(t)
			if tt.setup != nil {
				tt.setup(e)
			}
			if _, err := e.p.Restore(context.Background(), e.stack, RestoreOptions{SnapshotID: tt.id}); err == nil {
				t.Fatal("expected error")
			}
			for _, c := range e.calls.list() {
				if strings.HasPrefix(c, "docker.Stop") || strings.HasPrefix(c, "restic.Restore") {
					t.Fatalf("stack was touched: %v", e.calls.list())
				}
			}
		})
	}
}

func TestRestoreStartVariants(t *testing.T) {
	tests := []struct {
		name     string
		opts     RestoreOptions
		metadata *string // nil = remove metadata.txt from snapshot
		wantUp   string  // expected "docker.Up ..." call, "" = stack not started
	}{
		{"no metadata: start all", RestoreOptions{}, nil, "docker.Up <all>"},
		{"invalid metadata: start all", RestoreOptions{}, ptr("garbage"), "docker.Up <all>"},
		{"nothing was running", RestoreOptions{}, ptr("# stackvault-metadata v1\ncreated_at\t2026-11-01T00:00:00Z\nstack\tweb\napp\texited\t-\t-\t-\t-\n"), ""},
		{"no start requested", RestoreOptions{NoStart: true}, ptr(""), ""},
		{"skip safety snapshot", RestoreOptions{SkipSafetySnapshot: true}, ptr(""), "docker.Up app"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := restoreEnv(t)
			if tt.metadata == nil {
				delete(e.restic.restoreFiles, MetadataFile)
			} else if *tt.metadata != "" {
				e.restic.restoreFiles[MetadataFile] = *tt.metadata
			}
			tt.opts.SnapshotID = snapID[:8]
			if _, err := e.p.Restore(context.Background(), e.stack, tt.opts); err != nil {
				t.Fatal(err)
			}
			calls := e.calls.list()
			var gotUp string
			for _, c := range calls {
				if strings.HasPrefix(c, "docker.Up") || strings.HasPrefix(c, "docker.Start") {
					gotUp = c
				}
			}
			if gotUp != tt.wantUp {
				t.Fatalf("start call %q, want %q (all: %v)", gotUp, tt.wantUp, calls)
			}
			if tt.opts.SkipSafetySnapshot {
				for _, c := range calls {
					if strings.Contains(c, TagPreRestore) {
						t.Fatal("safety snapshot taken although disabled")
					}
				}
			}
			if got := readData(t, e.stack.Dir); got != "restored" {
				t.Errorf("stack dir has %q", got)
			}
		})
	}
}

func ptr(s string) *string { return &s }

// The manager restoring its own stack: its job files are carried over from
// the previous version into the restored stack before it starts.
func TestRestoreCarriesOverJobFiles(t *testing.T) {
	e := restoreEnv(t)
	jobDir := filepath.Join(e.stack.Dir, "data", "jobs", "J1")
	if err := os.MkdirAll(jobDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(jobDir, "status.json"), []byte("newest"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Started after the files are copied: the stack sees them at start.
	var seenAtStart string
	e.docker.onUp = func() {
		b, _ := os.ReadFile(filepath.Join(e.stack.Dir, "data", "jobs", "J1", "status.json"))
		seenAtStart = string(b)
	}
	res, err := e.p.Restore(context.Background(), e.stack, RestoreOptions{SnapshotID: snapID[:8], CarryOver: "data/jobs"})
	if err != nil {
		t.Fatal(err)
	}
	if seenAtStart != "newest" {
		t.Fatalf("job files at start: %q", seenAtStart)
	}
	if got := readData(t, e.stack.Dir); got != "restored" {
		t.Fatalf("stack not restored: %q", got)
	}
	fi, err := os.Stat(filepath.Join(e.stack.Dir, "data", "jobs", "J1", "status.json"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("carried-over file: %v %v", fi, err)
	}
	if _, err := os.Stat(filepath.Join(res.OldDir, "data", "jobs", "J1", "status.json")); err != nil {
		t.Fatal("previous version must keep its files")
	}
}

func TestValidCarryOver(t *testing.T) {
	for dir, want := range map[string]bool{
		"data/jobs": true, "jobs": true, "": false, ".": false, "../jobs": false,
		"/data/jobs": false, "data/../jobs": false, "data/jobs/": false,
	} {
		if got := ValidCarryOver(dir); got != want {
			t.Errorf("ValidCarryOver(%q) = %v", dir, got)
		}
	}
}
