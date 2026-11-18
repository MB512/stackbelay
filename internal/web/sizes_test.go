package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/MB512/stackvault/internal/restic"
)

func TestFormatDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		300 * time.Millisecond:      "<1 s",
		45 * time.Second:            "45 s",
		90 * time.Second:            "1 min 30 s",
		3 * time.Minute:             "3 min",
		2*time.Hour + 5*time.Minute: "2 h 5 min",
	} {
		if got := formatDuration(d); got != want {
			t.Errorf("formatDuration(%s) = %q, want %q", d, got, want)
		}
	}
}

// The Backups tab shows what each snapshot added and how long it took,
// and the stack size at the last backup.
func TestSnapshotSizes(t *testing.T) {
	e := newEnv(t, true, true)
	webID, _ := e.setupStacks()
	ctx := context.Background()
	repo, err := e.st.SetRepository(ctx, webID, "/srv/restic/web", "pw", nil)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 11, 13, 3, 0, 0, 0, time.UTC)
	if err := e.st.ReplaceSnapshots(ctx, repo, []restic.Snapshot{
		{ID: "aaaaaaaa11111111", ShortID: "aaaaaaaa", Time: t0, Hostname: "h", Paths: []string{"/s/web"},
			Summary: &restic.SnapshotSummary{BackupStart: t0, BackupEnd: t0.Add(90 * time.Second),
				DataAddedPacked: 2 << 20, TotalBytesProcessed: 315 << 20, TotalFilesProcessed: 1200}},
		{ID: "bbbbbbbb11111111", ShortID: "bbbbbbbb", Time: t0.Add(-24 * time.Hour), Hostname: "h", Paths: []string{"/s/web"}},
	}, t0); err != nil {
		t.Fatal(err)
	}
	e.loginAdmin()
	r := e.get("/stacks/web/backups")
	for _, want := range []string{
		"Stack size at the last backup: <strong>315.0 MiB</strong>", "1200 files", "the backup took 1 min 30 s",
		`<td title="315.0 MiB in 1200 files backed up">2.0 MiB</td>`, "<td>1 min 30 s</td>",
		`title="Made before restic 0.17: no size recorded">–</span>`, "not the size of\n    the stack",
	} {
		if !strings.Contains(r.body, want) {
			t.Errorf("Backups tab lacks %q", want)
		}
	}
	if t.Failed() {
		t.Log(r.body)
	}
}
