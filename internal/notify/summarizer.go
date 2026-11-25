package notify

import (
	"context"
	"log/slog"
	"time"

	"github.com/MB512/stackbelay/internal/docker"
	"github.com/MB512/stackbelay/internal/jobs"
	"github.com/MB512/stackbelay/internal/scheduler"
	"github.com/MB512/stackbelay/internal/stacks"
	"github.com/MB512/stackbelay/internal/store"
)

// SummaryStore is what the summary needs from the database (*store.Store).
type SummaryStore interface {
	NotifySettings(ctx context.Context) (store.NotifySettings, error)
	SummarySentAt(ctx context.Context) (time.Time, error)
	SetSummarySentAt(ctx context.Context, at time.Time) error
	StackSummaries(ctx context.Context) ([]store.StackSummary, error)
	MutedStacks(ctx context.Context) (map[int64]bool, error)
	StacksWithBackupSchedule(ctx context.Context) (map[int64]bool, error)
	StoppedOnPurpose(ctx context.Context) (map[int64]bool, error)
	JobsInState(ctx context.Context, state jobs.State) ([]store.Job, error)
	JobsFinishedBetween(ctx context.Context, from, to time.Time) ([]store.Job, error)
	AuditBetween(ctx context.Context, from, to time.Time) ([]store.AuditEntry, error)
	DataAddedBetween(ctx context.Context, from, to time.Time, host string) ([]store.AddedData, error)
}

// StackInspector reads a stack's state and checks for newer images
// (*docker.CLI; both read-only, see docs/security.md).
type StackInspector interface {
	StackOverview(ctx context.Context, dir string) (docker.Overview, error)
	CheckUpdates(ctx context.Context, dir string) ([]docker.ImageCheck, error)
}

// summaryCheckInterval is how often the Summarizer looks whether a summary
// is due.
const summaryCheckInterval = time.Minute

// Summarizer sends the periodic summary (issue #48). It runs as its own
// goroutine, because collecting (docker, registry checks) can take
// minutes and must not hold up event messages.
type Summarizer struct {
	Store  SummaryStore
	Sender Sender
	// Docker reads stack states and checks for updates; nil leaves both
	// out of the summary.
	Docker     StackInspector
	StacksRoot string
	// Hostname is STACKBELAY_HOSTNAME (restic --host of this host's
	// snapshots); messages show it shortened.
	Hostname string
	Logger   *slog.Logger
	Location *time.Location // default time.Local
	Now      func() time.Time

	failures    int
	nextAttempt time.Time
}

func (s *Summarizer) now() time.Time {
	if s.Now == nil {
		return time.Now()
	}
	return s.Now()
}

func (s *Summarizer) loc() *time.Location {
	if s.Location == nil {
		return time.Local
	}
	return s.Location
}

// Run sends summaries when they are due until ctx is cancelled.
func (s *Summarizer) Run(ctx context.Context) {
	ticker := time.NewTicker(summaryCheckInterval)
	defer ticker.Stop()
	for {
		s.Step(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Step sends the summary if it is due: the first run of its schedule after
// the last summary. A summary missed while the manager was down is sent
// at the next start and covers the whole gap. The first time (no summary
// sent yet), Step only starts the first period.
func (s *Summarizer) Step(ctx context.Context) {
	settings, err := s.Store.NotifySettings(ctx)
	if err != nil {
		s.Logger.Error("summary: reading settings", "error", err)
		return
	}
	target, on := TargetOf(settings)
	if !on || !Enabled(settings.Events, KeySummary) {
		return
	}
	sched, err := scheduler.Parse(settings.SummaryCron)
	if err != nil {
		s.Logger.Error("summary: invalid schedule", "cron", settings.SummaryCron, "error", err)
		return
	}
	now := s.now()
	last, err := s.Store.SummarySentAt(ctx)
	if err != nil {
		s.Logger.Error("summary: reading last summary", "error", err)
		return
	}
	if last.IsZero() {
		if err := s.Store.SetSummarySentAt(ctx, now); err != nil {
			s.Logger.Error("summary: starting the first period", "error", err)
		}
		return
	}
	if now.Before(sched.Next(last.In(s.loc()))) || now.Before(s.nextAttempt) {
		return
	}

	in, err := s.Collect(ctx, last, now, settings)
	if err != nil {
		s.Logger.Error("summary: collecting", "error", err)
		return
	}
	msg := FormatSummary(ShortHost(s.Hostname), BuildSummary(in), s.loc())
	sctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if err := s.Sender.Send(sctx, target, msg); err != nil {
		s.failures++
		backoff := backoffAfter(s.failures)
		s.nextAttempt = now.Add(backoff)
		s.Logger.Warn("summary: sending failed, retrying later", "retry_in", backoff.String(), "error", err)
		return
	}
	s.failures, s.nextAttempt = 0, time.Time{}
	if err := s.Store.SetSummarySentAt(ctx, now); err != nil {
		s.Logger.Error("summary: recording the sent summary", "error", err)
	}
	s.Logger.Info("summary sent", "from", last, "to", now)
}

// Collect gathers the data of the period [from, to).
func (s *Summarizer) Collect(ctx context.Context, from, to time.Time, settings store.NotifySettings) (SummaryInput, error) {
	in := SummaryInput{From: from, To: to, StaleAfter: time.Duration(max(settings.StaleDays, 1)) * 24 * time.Hour}
	var err error
	if in.Jobs, err = s.Store.JobsFinishedBetween(ctx, from, to); err != nil {
		return in, err
	}
	if in.Audit, err = s.Store.AuditBetween(ctx, from, to); err != nil {
		return in, err
	}
	if in.Added, err = s.Store.DataAddedBetween(ctx, from, to, s.Hostname); err != nil {
		return in, err
	}
	sums, err := s.Store.StackSummaries(ctx)
	if err != nil {
		return in, err
	}
	muted, err := s.Store.MutedStacks(ctx)
	if err != nil {
		return in, err
	}
	scheduled, err := s.Store.StacksWithBackupSchedule(ctx)
	if err != nil {
		return in, err
	}
	onPurpose, err := s.Store.StoppedOnPurpose(ctx)
	if err != nil {
		return in, err
	}
	running, err := s.Store.JobsInState(ctx, jobs.StateRunning)
	if err != nil {
		return in, err
	}
	busy := map[int64]bool{}
	for _, j := range running {
		busy[j.StackID] = true
	}
	for _, sum := range sums {
		if !sum.Present {
			continue // vanished, or imported and not restored yet
		}
		f := StackFacts{Name: sum.Name, Muted: muted[sum.ID], HasRepository: sum.HasRepository,
			HasBackupSchedule: scheduled[sum.ID], LastBackup: sum.LastBackup, LastGood: sum.LastFinal.Time,
			Busy: busy[sum.ID], StoppedOnPurpose: onPurpose[sum.ID]}
		if !f.Muted {
			s.inspect(ctx, sum.Name, &f)
		}
		in.Stacks = append(in.Stacks, f)
	}
	return in, nil
}

// inspect adds what only the stack directory and docker know. Failures
// leave the facts out; the summary is sent anyway.
func (s *Summarizer) inspect(ctx context.Context, name string, f *StackFacts) {
	if s.StacksRoot == "" {
		return
	}
	st, err := stacks.Lookup(s.StacksRoot, name)
	if err != nil {
		return
	}
	f.RestoreIncomplete = stacks.RestoreIncomplete(st.Dir)
	if s.Docker == nil || f.Busy {
		return
	}
	octx, cancel := context.WithTimeout(ctx, 30*time.Second)
	ov, err := s.Docker.StackOverview(octx, st.Dir)
	cancel()
	if err != nil {
		s.Logger.Warn("summary: reading stack state", "stack", name, "error", err)
		return
	}
	f.Condition, f.Problems = ov.Condition()
	if f.Condition == docker.ConditionStopped || f.Condition == docker.ConditionNotCreated {
		return // nothing running to update
	}
	uctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	checks, err := s.Docker.CheckUpdates(uctx, st.Dir)
	cancel()
	if err != nil {
		s.Logger.Warn("summary: checking for updates", "stack", name, "error", err)
		return
	}
	for _, c := range checks {
		if c.Running && c.Status == docker.UpdateAvailable {
			f.UpdatesAvailable++
		}
	}
}
