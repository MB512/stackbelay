package web

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MB512/stackvault/internal/jobs"
	"github.com/MB512/stackvault/internal/restic"
	"github.com/MB512/stackvault/internal/store"
)

// A running job shows its phase and transfer progress (progress.json of
// the worker); a finished job does not.
func TestJobPageProgress(t *testing.T) {
	e := newEnv(t, true, true)
	web, _ := e.setupStacks()
	ctx := context.Background()
	const id = "01M7M8Q4T1R4WKXGJEHRRFD980"
	if err := e.st.CreateJob(ctx, store.Job{ID: id, StackID: web, Type: jobs.TypeBackup,
		Trigger: store.TriggerManual, QueuedAt: e.clock()}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := e.st.ClaimNextJob(ctx, 1, e.clock()); !ok || err != nil {
		t.Fatalf("claim: %v %v", ok, err)
	}
	dir := filepath.Join(e.server.JobsDir, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	prog := &jobs.FileProgress{Dir: dir}
	prog.Phase("final snapshot")
	prog.Transfer(restic.Progress{Percent: 0.42, BytesDone: 420 << 20, TotalBytes: 1000 << 20,
		FilesDone: 3, TotalFiles: 7, SecondsElapsed: 10, SecondsRemaining: 14})
	e.loginAdmin()

	r := e.get("/jobs/" + id)
	expect(t, r, 200, "")
	for _, want := range []string{"<strong>final snapshot</strong>", `<progress max="1" value="0.42">`,
		"42 % · 420.0 MiB of 1000.0 MiB · 3 of 7 files · 42.0 MiB/s · about 14 s left"} {
		if !strings.Contains(r.body, want) {
			t.Errorf("job page lacks %q", want)
		}
	}

	// A phase without transfer (e.g. waiting until healthy) shows no bar.
	prog.Phase("waiting until healthy")
	prog.Detail("waiting for: app")
	r = e.get("/jobs/" + id)
	if !strings.Contains(r.body, `<strong>waiting until healthy</strong> <span class="hint">waiting for: app</span>`) ||
		strings.Contains(r.body, "<progress") {
		t.Fatalf("phase without transfer:\n%s", r.body)
	}

	if err := e.st.FinishJob(ctx, id, jobs.StateSucceeded, "aaaaaaaa", "", store.JobResult{}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if r := e.get("/jobs/" + id); strings.Contains(r.body, "waiting until healthy") {
		t.Fatal("progress shown for a finished job")
	}
}

func TestFormatBytesAndSeconds(t *testing.T) {
	for n, want := range map[uint64]string{0: "0 B", 1023: "1023 B", 1536: "1.5 KiB", 5 << 30: "5.0 GiB"} {
		if got := formatBytes(n); got != want {
			t.Errorf("formatBytes(%d) = %q, want %q", n, got, want)
		}
	}
	for s, want := range map[int64]string{45: "45 s", 150: "3 min", 7500: "2 h 5 min"} {
		if got := formatSeconds(s); got != want {
			t.Errorf("formatSeconds(%d) = %q, want %q", s, got, want)
		}
	}
}
