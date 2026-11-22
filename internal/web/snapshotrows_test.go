package web

import (
	"testing"
	"time"

	"github.com/MB512/stackbelay/internal/restic"
	"github.com/MB512/stackbelay/internal/store"
)

func snap(id, tag string, added uint64, dur time.Duration) store.CachedSnapshot {
	t0 := time.Date(2026, 11, 19, 3, 0, 0, 0, time.UTC)
	sn := store.CachedSnapshot{Snapshot: restic.Snapshot{ID: id, ShortID: id, Tags: []string{tag}}}
	if added > 0 {
		sn.Summary = &restic.SnapshotSummary{BackupStart: t0, BackupEnd: t0.Add(dur), DataAddedPacked: added}
	}
	return sn
}

func TestSnapshotRows(t *testing.T) {
	// Newest first, as store.StackSnapshots returns them.
	snaps := []store.CachedSnapshot{
		snap("p4", "pre-stop", 5, 5*time.Second), // newest: its backup failed or still runs
		snap("f3", "final", 10, 2*time.Second),
		snap("p3", "pre-stop", 300, time.Minute),
		snap("p2", "pre-stop", 0, 0), // failed backup's pre-stop, no summary (restic < 0.17)
		snap("r1", "pre-restore", 50, 30*time.Second),
		snap("f1", "final", 7, time.Second),
		snap("p1", "pre-stop", 100, 20*time.Second),
	}
	rows, hidden := snapshotRows(snaps, false)
	type want struct {
		id       string
		preStops int
		added    uint64
		dur      time.Duration
		downtime time.Duration
	}
	wants := []want{
		{"f3", 2, 310, time.Minute + 2*time.Second, 2 * time.Second},
		{"r1", 0, 50, 30 * time.Second, 30 * time.Second},
		{"f1", 1, 107, 21 * time.Second, time.Second},
	}
	if len(rows) != len(wants) || hidden != 1 {
		t.Fatalf("rows %+v, hidden %d", rows, hidden)
	}
	for i, w := range wants {
		r := rows[i]
		if r.ID != w.id || r.PreStops != w.preStops || r.Added != w.added || r.Duration != w.dur ||
			r.Downtime != w.downtime || r.PreStop || !r.HasSize {
			t.Errorf("row %d = %+v, want %+v", i, r, w)
		}
	}

	all, hidden := snapshotRows(snaps, true)
	if len(all) != len(snaps) || hidden != 0 {
		t.Fatalf("with pre-stop: %d rows, %d hidden", len(all), hidden)
	}
	if !all[0].PreStop || all[0].Added != 5 || all[0].PreStops != 0 || all[1].Added != 10 || all[3].HasSize {
		t.Errorf("rows with pre-stop: %+v", all)
	}
}
