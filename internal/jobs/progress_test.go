package jobs

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/MB512/stackbelay/internal/restic"
)

// fakeProgress records the phases and details a pipeline reports.
type fakeProgress struct {
	mu      sync.Mutex
	phases  []string
	details []string
}

func (f *fakeProgress) Phase(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.phases = append(f.phases, name)
}

func (f *fakeProgress) Detail(d string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.details = append(f.details, d)
}

func (f *fakeProgress) Transfer(restic.Progress) {}

func TestBackupPhases(t *testing.T) {
	e := newEnv(t)
	prog := &fakeProgress{}
	e.p.Progress = prog
	if _, err := e.p.Backup(context.Background(), e.stack); err != nil {
		t.Fatal(err)
	}
	want := []string{"pre-stop snapshot", "stopping stack", "final snapshot", "starting stack", "waiting until healthy"}
	if !reflect.DeepEqual(prog.phases, want) {
		t.Fatalf("phases %q, want %q", prog.phases, want)
	}
}

// The phases of a rollback carry the prefix "rollback: ".
func TestUpdateRollbackPhases(t *testing.T) {
	e := rollbackEnv(t)
	e.docker.upFailures = 1
	prog := &fakeProgress{}
	e.p.Progress = prog
	if _, err := e.p.Update(context.Background(), e.stack, UpdateOptions{Backup: true}); err == nil {
		t.Fatal("update did not fail")
	}
	want := []string{"pulling images", "pre-stop snapshot", "stopping stack", "final snapshot", "recreating services",
		"rollback", "rollback: stopping stack", "rollback: restoring", "rollback: starting stack",
		"rollback: waiting until healthy"}
	if !reflect.DeepEqual(prog.phases, want) {
		t.Fatalf("phases\n got %q\nwant %q", prog.phases, want)
	}
	if e.p.phasePrefix != "" {
		t.Fatalf("prefix left over: %q", e.p.phasePrefix)
	}
}

func TestFileProgress(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	f := &FileProgress{Dir: dir, Now: func() time.Time { return now }}

	if _, err := ReadProgress(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("no file yet: %v", err)
	}
	f.Phase("final snapshot")
	f.Transfer(restic.Progress{Percent: 0.1, BytesDone: 10, TotalBytes: 100, SecondsElapsed: 2})
	got, err := ReadProgress(dir)
	if err != nil || got.Phase != "final snapshot" || got.Transfer == nil || got.Transfer.BytesDone != 10 ||
		!got.UpdatedAt.Equal(now) {
		t.Fatalf("after first transfer: %+v, %v", got, err)
	}
	if got.Transfer.BytesPerSecond() != 5 {
		t.Fatalf("speed %d", got.Transfer.BytesPerSecond())
	}

	// Within a second, transfer updates are kept in memory only.
	now = now.Add(500 * time.Millisecond)
	f.Transfer(restic.Progress{Percent: 0.2, BytesDone: 20, TotalBytes: 100})
	if got, _ := ReadProgress(dir); got.Transfer.BytesDone != 10 {
		t.Fatalf("written within the interval: %+v", got.Transfer)
	}
	// A detail is written at once and keeps the transfer.
	f.Detail("waiting for: app")
	if got, _ := ReadProgress(dir); got.Detail != "waiting for: app" || got.Transfer.BytesDone != 20 {
		t.Fatalf("after detail: %+v", got)
	}
	// A new phase clears detail and transfer.
	now = now.Add(2 * time.Second)
	f.Phase("starting stack")
	if got, _ := ReadProgress(dir); got.Phase != "starting stack" || got.Detail != "" || got.Transfer != nil {
		t.Fatalf("after new phase: %+v", got)
	}

	// The file is replaced atomically (no temporary file left over).
	if _, err := os.Stat(filepath.Join(dir, ProgressFile+".tmp")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("temporary file left: %v", err)
	}
}
