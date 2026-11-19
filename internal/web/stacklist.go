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

// stackRow is one line of the stack list.
type stackRow struct {
	store.StackSummary
	BackupSchedule, UpdateSchedule scheduleInfo
	Check                          lastCheck
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
