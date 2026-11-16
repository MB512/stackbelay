package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func inPlaceOpts() RestoreOptions { return RestoreOptions{SnapshotID: snapID[:8], InPlace: true} }

// assertNoSideDirs checks that an in-place restore created neither a
// staging nor a previous-version directory.
func assertNoSideDirs(t *testing.T, e *env) {
	t.Helper()
	assertNoStaging(t, e)
	if old, _ := filepath.Glob(e.stack.Dir + ".old-*"); len(old) > 0 {
		t.Fatalf("previous-version directory created: %v", old)
	}
}

func TestRestoreInPlaceSuccess(t *testing.T) {
	e := restoreEnv(t)
	stale := filepath.Join(e.stack.Dir, "stale.txt")
	if err := os.WriteFile(stale, []byte("created after the snapshot"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := e.p.Restore(context.Background(), e.stack, inPlaceOpts())
	if err != nil {
		t.Fatal(err)
	}
	assertCalls(t, e.calls.list(), []string{
		"restic.CheckRepo",
		"restic.Snapshots " + snapID[:8],
		"docker.Services",
		"docker.Stop",
		"restic.Backup tag=pre-restore parent= host=host-a",
		"restic.RestoreInPlace " + snapID + ":/home/docker/web exclude=",
		"restic.Node " + snapID + ":/home/docker/web",
		"docker.Up app",
		"docker.WaitHealthy",
	})
	if res.OldDir != "" || res.SafetySnapshot != "snap-pre-restore" || res.Snapshot != snapID {
		t.Errorf("unexpected result %+v", res)
	}
	if got := readData(t, e.stack.Dir); got != "restored" {
		t.Errorf("stack dir has %q", got)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("file not in the snapshot kept: %v", err)
	}
	fi, err := os.Stat(e.stack.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o750 || !fi.ModTime().Equal(e.restic.node.ModTime) {
		t.Errorf("stack dir metadata not restored: mode %v, mtime %v", fi.Mode(), fi.ModTime())
	}
	assertNoSideDirs(t, e)
}

// Before restic writes into the stack directory, a failure restarts the
// previous state exactly like a restore with swap.
func TestRestoreInPlaceFailureBeforeWriting(t *testing.T) {
	e := restoreEnv(t)
	e.restic.failOn["Backup:pre-restore"] = errBoom
	_, err := e.p.Restore(context.Background(), e.stack, inPlaceOpts())
	if !errors.Is(err, errBoom) {
		t.Fatalf("expected errBoom, got %v", err)
	}
	assertCalls(t, e.calls.list(), []string{"restic.CheckRepo", "restic.Snapshots " + snapID[:8], "docker.Services",
		"docker.Stop", "restic.Backup tag=pre-restore parent= host=host-a", "docker.Start app,db", "docker.WaitHealthy"})
	if got := readData(t, e.stack.Dir); got != "original" {
		t.Errorf("stack directory was modified: %q", got)
	}
	if strings.Contains(err.Error(), "partly restored") {
		t.Errorf("untouched stack reported as partly restored: %v", err)
	}
}

// Once restic has written into the stack directory, the previous state is
// gone: the stack must stay stopped (no start of a half-restored stack) and
// the error must say how to get back.
func TestRestoreInPlacePartialFailure(t *testing.T) {
	tests := []struct {
		name       string
		setup      func(e *env)
		opts       RestoreOptions
		wantInErr  []string
		lastCall   string
		notStarted bool
	}{
		{
			name:      "restic restore fails",
			setup:     func(e *env) { e.restic.failOn["RestoreInPlace"] = errBoom },
			opts:      inPlaceOpts(),
			wantInErr: []string{"partly restored", "left stopped", "safety snapshot snap-pre"},
			lastCall:  "restic.RestoreInPlace " + snapID + ":/home/docker/web exclude=",
		},
		{
			name:      "reading directory metadata fails",
			setup:     func(e *env) { e.restic.failOn["Node"] = errBoom },
			opts:      inPlaceOpts(),
			wantInErr: []string{"partly restored", "safety snapshot snap-pre"},
			lastCall:  "restic.Node " + snapID + ":/home/docker/web",
		},
		{
			name:      "without safety snapshot",
			setup:     func(e *env) { e.restic.failOn["RestoreInPlace"] = errBoom },
			opts:      RestoreOptions{SnapshotID: snapID[:8], InPlace: true, SkipSafetySnapshot: true},
			wantInErr: []string{"partly restored", "no safety snapshot", "run the restore again"},
			lastCall:  "restic.RestoreInPlace " + snapID + ":/home/docker/web exclude=",
		},
		{
			name: "pinned image missing",
			setup: func(e *env) {
				e.restic.restoreFiles[MetadataFile] = pinMetadata
				e.docker.failOn["EnsureImage"] = errBoom
			},
			opts:      RestoreOptions{SnapshotID: snapID[:8], InPlace: true, PinImages: true},
			wantInErr: []string{"is restored, but the stack was not started"},
			lastCall:  "docker.EnsureImage " + testDigest,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := restoreEnv(t)
			tt.setup(e)
			_, err := e.p.Restore(context.Background(), e.stack, tt.opts)
			if !errors.Is(err, errBoom) {
				t.Fatalf("expected errBoom, got %v", err)
			}
			for _, want := range tt.wantInErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not contain %q", err, want)
				}
			}
			calls := e.calls.list()
			if calls[len(calls)-1] != tt.lastCall {
				t.Fatalf("stack touched after the failure (want last call %q):\n  %s", tt.lastCall, strings.Join(calls, "\n  "))
			}
			assertNoSideDirs(t, e)
		})
	}
}

// A failing start after a complete in-place restore is reported as such,
// without the advice for a partial restore.
func TestRestoreInPlaceStartFails(t *testing.T) {
	e := restoreEnv(t)
	e.docker.failOn["Up"] = errBoom
	_, err := e.p.Restore(context.Background(), e.stack, inPlaceOpts())
	if !errors.Is(err, errBoom) || !strings.Contains(err.Error(), "starting restored stack") {
		t.Fatalf("unexpected error %v", err)
	}
	if strings.Contains(err.Error(), "partly restored") || strings.Contains(err.Error(), "not started") {
		t.Errorf("misleading error: %v", err)
	}
	if got := readData(t, e.stack.Dir); got != "restored" {
		t.Errorf("stack dir has %q", got)
	}
}

// The manager restoring its own stack in place: its job directory is
// excluded from the restore, so the current job files stay as they are.
func TestRestoreInPlaceExcludesCarryOver(t *testing.T) {
	e := restoreEnv(t)
	jobFile := filepath.Join(e.stack.Dir, "data", "jobs", "J1", "status.json")
	if err := os.MkdirAll(filepath.Dir(jobFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jobFile, []byte("newest"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := inPlaceOpts()
	opts.CarryOver = "data/jobs"
	if _, err := e.p.Restore(context.Background(), e.stack, opts); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range e.calls.list() {
		found = found || c == "restic.RestoreInPlace "+snapID+":/home/docker/web exclude=data/jobs"
	}
	if !found {
		t.Fatalf("job directory not excluded: %v", e.calls.list())
	}
	if b, err := os.ReadFile(jobFile); err != nil || string(b) != "newest" {
		t.Fatalf("job file: %q %v", b, err)
	}
	assertNoSideDirs(t, e)
}

func TestRestoreInPlaceNoStart(t *testing.T) {
	e := restoreEnv(t)
	opts := inPlaceOpts()
	opts.NoStart = true
	if _, err := e.p.Restore(context.Background(), e.stack, opts); err != nil {
		t.Fatal(err)
	}
	for _, c := range e.calls.list() {
		if strings.HasPrefix(c, "docker.Up") || strings.HasPrefix(c, "docker.Start") {
			t.Fatalf("stack started: %v", e.calls.list())
		}
	}
	if got := readData(t, e.stack.Dir); got != "restored" {
		t.Errorf("stack dir has %q", got)
	}
}

func TestRestoreRejectsInvalidCarryOver(t *testing.T) {
	e := restoreEnv(t)
	opts := inPlaceOpts()
	opts.CarryOver = "../other"
	if _, err := e.p.Restore(context.Background(), e.stack, opts); err == nil {
		t.Fatal("expected error")
	}
	if len(e.calls.list()) != 0 {
		t.Fatalf("stack touched: %v", e.calls.list())
	}
}
