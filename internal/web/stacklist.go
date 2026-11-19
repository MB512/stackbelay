package web

import (
	"fmt"
	"time"

	"github.com/MB512/stackvault/internal/jobs"
	"github.com/MB512/stackvault/internal/scheduler"
	"github.com/MB512/stackvault/internal/store"
)

// The stack list (issue #37): per stack a Backup group (scheduling,
// repository, last backup) and an Update group (scheduling, last check,
// last update), with relative times and status dots.

// stackListPage is the data of the stack list. Stacks whose directory
// vanished are hidden unless ShowAll (issue #27, "?all=1": bookmarkable,
// works without JavaScript); Hidden counts them.
type stackListPage struct {
	Rows    []stackRow
	ShowAll bool
	Hidden  int
}

// vanished reports whether st's directory existed once and is gone now.
// A stack imported from another host that was never restored here is not
// vanished: it is shown as "not restored yet".
func vanished(st store.Stack) bool { return !st.Present && st.SeenPresent }

// stackRow is one line of the stack list.
type stackRow struct {
	store.StackSummary
	BackupSchedule, UpdateSchedule scheduleInfo
	Backup                         backupCell
	Check                          lastCheck
}

// backupCell is the "Last" cell of the backup group: the newest good final
// snapshot (made by a backup, update or compose apply), or, if a newer
// backup job failed or still runs, that job with the time of the last
// good one.
type backupCell struct {
	Job   store.JobBrief // the job shown, with its dot and link; zero: never backed up
	Kind  string         // what made the final snapshot: "backup", "update", "compose change"
	Final bool           // Job is the good final snapshot (dot ✓)
	// LastGood is the last good final snapshot when Job is a newer backup
	// that failed or runs; zero if there was none.
	LastGood time.Time
}

// newBackupCell decides what the backup cell shows.
func newBackupCell(lastBackup store.JobBrief, final store.FinalBackup) backupCell {
	if final.ID != "" && (lastBackup.ID == "" || !final.Time.Before(lastBackup.Time) || lastBackup.State == jobs.StateSucceeded) {
		kind := map[jobs.Type]string{jobs.TypeUpdate: "update", jobs.TypeComposeApply: "compose change"}[final.Type]
		if kind == "" {
			kind = "backup"
		}
		// A rolled-back update failed, but its final snapshot is good.
		job := final.JobBrief
		job.State = jobs.StateSucceeded
		return backupCell{Job: job, Kind: kind, Final: true}
	}
	return backupCell{Job: lastBackup, Kind: "backup", LastGood: final.Time}
}

// scheduleInfo summarizes the schedules of one job type of a stack.
type scheduleInfo struct {
	Count   int       // schedules of this type
	Enabled int       // enabled ones
	Next    time.Time // next run of the enabled ones; zero if none
	Cron    string    // expression of the schedule that runs next
}

// lastCheck is the newest look for new images: the newest update job (any
// outcome, stored) or the newest manual registry check (in memory, lost
// on restart), whichever is newer.
type lastCheck struct {
	Time time.Time // zero: never checked
	// Job is set if the newest check is an update job.
	Job store.JobBrief
	// Registry is set if the newest check is a manual registry check.
	Registry *updateResult
}

// UpdateAvailable reports whether the newest check is a registry check
// that found newer images or pinned services.
func (c lastCheck) UpdateAvailable() bool {
	return c.Registry != nil && c.Registry.Error == "" && (c.Registry.NewImages > 0 || len(c.Registry.Pinned) > 0)
}

// stackRows builds the rows of sums.
func (s *Server) stackRows(sums []store.StackSummary, schedules []store.Schedule) []stackRow {
	now := s.now().In(s.loc()) // cron expressions run in the manager's time zone
	type key struct {
		stack int64
		typ   jobs.Type
	}
	infos := map[key]scheduleInfo{}
	for _, sc := range schedules {
		k := key{sc.StackID, sc.Type}
		info := infos[k]
		info.Count++
		if sc.Enabled {
			info.Enabled++
			if sched, err := scheduler.Parse(sc.CronExpr); err == nil {
				if next := sched.Next(now); info.Next.IsZero() || next.Before(info.Next) {
					info.Next, info.Cron = next, sc.CronExpr
				}
			}
		}
		infos[k] = info
	}
	rows := make([]stackRow, 0, len(sums))
	for _, sum := range sums {
		row := stackRow{
			StackSummary:   sum,
			BackupSchedule: infos[key{sum.ID, jobs.TypeBackup}],
			UpdateSchedule: infos[key{sum.ID, jobs.TypeUpdate}],
			Backup:         newBackupCell(sum.LastBackup, sum.LastFinal),
			Check:          lastCheck{Time: sum.LastUpdate.Time, Job: sum.LastUpdate},
		}
		if res := s.updateResults.get(sum.ID); res != nil && res.At.After(row.Check.Time) {
			row.Check = lastCheck{Time: res.At, Registry: res}
		}
		rows = append(rows, row)
	}
	return rows
}

// formatAge shows how long ago (or until) something is, roughly: "45 s",
// "12 min", "4 h", "25 d", "1 y" (issue #37). Negative durations count
// as 0.
func formatAge(d time.Duration) string {
	d = max(d, 0)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d h", int(d/time.Hour))
	case d < 365*24*time.Hour:
		return fmt.Sprintf("%d d", int(d/(24*time.Hour)))
	default:
		return fmt.Sprintf("%d y", int(d/(365*24*time.Hour)))
	}
}
