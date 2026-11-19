package web

import (
	"slices"
	"time"

	"github.com/MB512/stackvault/internal/jobs"
	"github.com/MB512/stackvault/internal/store"
)

// snapshotRow is one line of the snapshot list on the Backups tab
// (issue #35). By default pre-stop snapshots are not listed: each
// remaining snapshot shows its own size added and duration plus those of
// the pre-stop snapshots right before it (usually the one of the same
// job), so a row says what the backup cost. With "show pre-stop
// snapshots" every snapshot is a row of its own, without sums.
type snapshotRow struct {
	store.CachedSnapshot
	PreStop bool // a pre-stop snapshot (only listed when asked for)
	// PreStops is the number of hidden pre-stop snapshots added to this row.
	PreStops int
	// HasSize: the row's own snapshot has a summary (restic >= 0.17).
	HasSize bool
	// Added and Duration are the sums over the snapshot and its PreStops
	// (pre-stop snapshots without a summary count as 0).
	Added    uint64
	Duration time.Duration
	// Downtime is the duration of the snapshot itself: for a final
	// snapshot after a pre-stop snapshot, the time the stack was stopped.
	Downtime time.Duration
}

// isPreStop reports whether the snapshot is a pre-stop snapshot (taken
// while the stack ran, not consistent).
func isPreStop(sn store.CachedSnapshot) bool { return slices.Contains(sn.Tags, jobs.TagPreStop) }

// snapshotRows builds the rows from snaps (newest first, as
// store.StackSnapshots returns them). It also returns the number of
// pre-stop snapshots that are hidden: those newer than the newest other
// snapshot (a backup that failed after its pre-stop snapshot, or one that
// is still running) have no row to be added to.
func snapshotRows(snaps []store.CachedSnapshot, showPreStop bool) (rows []snapshotRow, hidden int) {
	if showPreStop {
		for _, sn := range snaps {
			rows = append(rows, ownRow(sn))
		}
		return rows, 0
	}
	// Oldest first, so the pre-stop snapshots before a snapshot are known
	// when it comes.
	var pending []store.CachedSnapshot
	for _, sn := range slices.Backward(snaps) {
		if isPreStop(sn) {
			pending = append(pending, sn)
			continue
		}
		row := ownRow(sn)
		row.PreStops = len(pending)
		for _, p := range pending {
			if p.Summary != nil {
				row.Added += p.Summary.DataAddedPacked
				row.Duration += p.Summary.Duration()
			}
		}
		pending = nil
		rows = append(rows, row)
	}
	slices.Reverse(rows)
	return rows, len(pending)
}

func ownRow(sn store.CachedSnapshot) snapshotRow {
	row := snapshotRow{CachedSnapshot: sn, PreStop: isPreStop(sn), HasSize: sn.Summary != nil}
	if sn.Summary != nil {
		row.Added = sn.Summary.DataAddedPacked
		row.Duration = sn.Summary.Duration()
		row.Downtime = row.Duration
	}
	return row
}
