package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MB512/stackbelay/internal/docker"
	"github.com/MB512/stackbelay/internal/jobs"
	"github.com/MB512/stackbelay/internal/store"
)

var (
	periodFrom = time.Date(2026, 11, 18, 7, 0, 0, 0, time.UTC) // a Wednesday
	periodTo   = periodFrom.Add(7 * 24 * time.Hour)
)

func ptr(t time.Time) *time.Time { return &t }

// healthy is a stack with nothing to report.
func healthy(name string) StackFacts {
	return StackFacts{Name: name, HasRepository: true, HasBackupSchedule: true,
		LastBackup: store.JobBrief{ID: "x", State: jobs.StateSucceeded}, LastGood: periodTo.Add(-6 * time.Hour),
		Condition: docker.ConditionStarted}
}

func TestSummaryAttention(t *testing.T) {
	with := func(name string, change func(*StackFacts)) StackFacts {
		f := healthy(name)
		change(&f)
		return f
	}
	in := SummaryInput{From: periodFrom, To: periodTo, StaleAfter: 48 * time.Hour, Stacks: []StackFacts{
		healthy("ok"),
		with("immich", func(f *StackFacts) {
			f.LastBackup.State, f.LastGood = jobs.StateFailed, periodTo.Add(-72*time.Hour)
		}),
		with("paperless", func(f *StackFacts) { f.HasBackupSchedule = false }),
		with("norepo", func(f *StackFacts) { f.HasRepository = false }),
		with("stale", func(f *StackFacts) { f.LastGood = periodTo.Add(-5 * 24 * time.Hour) }),
		with("never", func(f *StackFacts) { f.LastGood = time.Time{} }),
		with("nextcloud", func(f *StackFacts) {
			f.Condition, f.Problems = docker.ConditionDegraded, []string{"redis is not running"}
		}),
		with("crashed", func(f *StackFacts) { f.Condition = docker.ConditionStopped }),
		with("paused", func(f *StackFacts) { f.Condition, f.StoppedOnPurpose = docker.ConditionStopped, true }),
		with("busy", func(f *StackFacts) { f.Condition, f.Busy = docker.ConditionStopped, true }),
		with("halfway", func(f *StackFacts) { f.RestoreIncomplete = true }),
		with("gitea", func(f *StackFacts) { f.UpdatesAvailable = 1 }),
		with("vaultwarden", func(f *StackFacts) { f.UpdatesAvailable = 2 }),
		with("muted", func(f *StackFacts) { f.Muted, f.HasRepository = true, false }),
	}}
	s := BuildSummary(in)
	want := []string{
		"immich: last backup failed, last good 3 d ago",
		"paperless: no backup schedule",
		"norepo: no repository",
		"stale: no backup for 5 d",
		"never: never backed up",
		"nextcloud: degraded (redis is not running)",
		"crashed: stopped",
		"halfway: restore incomplete",
		"updates available: gitea, vaultwarden",
	}
	if !slices.Equal(s.Attention, want) {
		t.Errorf("attention:\n  %s", strings.Join(s.Attention, "\n  "))
	}
	if s.Stacks != 13 {
		t.Errorf("stacks %d, want 13 (muted left out)", s.Stacks)
	}
}

func TestSummaryCounts(t *testing.T) {
	stopped := periodFrom.Add(time.Hour)
	job := func(stack string, typ jobs.Type, state jobs.State, res store.JobResult) store.Job {
		return store.Job{StackName: stack, Type: typ, State: state, Result: res, FinishedAt: stopped.Add(20 * time.Minute)}
	}
	down := func(d time.Duration) store.JobResult {
		return store.JobResult{StackStoppedAt: ptr(stopped), StackStartedAt: ptr(stopped.Add(d))}
	}
	in := SummaryInput{From: periodFrom, To: periodTo, StaleAfter: 48 * time.Hour,
		Jobs: []store.Job{
			job("web", jobs.TypeBackup, jobs.StateSucceeded, down(2*time.Minute)),
			job("immich", jobs.TypeBackup, jobs.StateSucceeded, down(14*time.Minute)),
			job("db", jobs.TypeBackup, jobs.StateFailed, store.JobResult{StackStoppedAt: ptr(stopped)}), // not started: until finished
			job("db", jobs.TypeBackup, jobs.StateCancelled, store.JobResult{}),
			job("immich", jobs.TypeUpdate, jobs.StateSucceeded, store.JobResult{ImagesChanged: []string{"server", "ml"}}),
			job("web", jobs.TypeUpdate, jobs.StateSucceeded, store.JobResult{Unchanged: true}),
			job("gitea", jobs.TypeUpdate, jobs.StateFailed, store.JobResult{RolledBack: true}),
			job("nextcloud", jobs.TypeComposeApply, jobs.StateSucceeded, store.JobResult{}),
			job("paperless", jobs.TypeRestore, jobs.StateSucceeded, down(time.Hour)), // restores do not count as downtime
			job("web", jobs.TypeStop, jobs.StateSucceeded, store.JobResult{}),
		},
		Audit: []store.AuditEntry{
			{Action: "login.failure"}, {Action: "login.failure"}, {Action: "password.change_failed"},
			{Action: "user.create"}, {Action: "permission.update"},
			{Action: "shell.start", StackName: "nextcloud"}, {Action: "shell.end", StackName: "nextcloud"},
			{Action: "schedule.create"}, {Action: "schedule.delete"}, {Action: "repository.update"},
			{Action: "job.create"}, {Action: "login.success"},
		},
		Added: []store.AddedData{
			{Repository: "offsite", Stack: "web", Bytes: 410 << 20},
			{Repository: "nas-2026", Stack: "immich", Bytes: 2900 << 20},
			{Repository: "nas-2026", Stack: "web", Bytes: 500 << 20},
		},
	}
	s := BuildSummary(in)
	if s.BackupsOK != 2 || s.BackupsFailed != 1 || s.BackupsCancelled != 1 {
		t.Errorf("backups %d/%d/%d", s.BackupsOK, s.BackupsFailed, s.BackupsCancelled)
	}
	if s.Downtime != 36*time.Minute || s.Longest != 20*time.Minute || s.LongestStack != "db" {
		t.Errorf("downtime %v, longest %v %s", s.Downtime, s.Longest, s.LongestStack)
	}
	if s.UpdatesApplied != 1 || s.UpdatesUnchanged != 1 || s.UpdatesRolledBack != 1 ||
		!slices.Equal(s.UpdateLines, []string{"immich: server, ml", "gitea: rolled back"}) {
		t.Errorf("updates %+v", s)
	}
	if !slices.Equal(s.ComposeEdits, []string{"nextcloud"}) || !slices.Equal(s.Restores, []string{"paperless"}) {
		t.Errorf("changes %v, restores %v", s.ComposeEdits, s.Restores)
	}
	if s.SettingsChanges["schedules"] != 2 || s.SettingsChanges["repositories"] != 1 || len(s.SettingsChanges) != 2 {
		t.Errorf("settings %v", s.SettingsChanges)
	}
	if s.FailedLogins != 3 || s.UserChanges != 2 || s.ShellSessions != 1 || !slices.Equal(s.ShellStacks, []string{"nextcloud"}) {
		t.Errorf("security %+v", s)
	}
	if len(s.Added) != 2 || s.Added[0] != (RepoAdded{"nas-2026", 3400 << 20, "immich", 2900 << 20}) || s.Added[1].Repository != "offsite" {
		t.Errorf("added %+v", s.Added)
	}

	m := FormatSummary("nas01", s, time.UTC)
	wantLines := []string{
		"All 0 stacks OK.",
		"",
		"📦 Backups: 2 ok · 1 failed · 1 cancelled",
		"⏱ Downtime: total 36 min · longest: db 20 min (backup, Wed)",
		"🔄 Updates: 1 applied · 1 unchanged · 1 rolled back",
		"  - immich: server, ml",
		"  - gitea: rolled back",
		"🛠 Changes: 1 compose edit (nextcloud) · settings: 1 repositories, 2 schedules",
		"♻️ Restores: 1 (paperless)",
		"🔐 Security: 3 failed logins · 2 user/permission changes · 1 shell session (nextcloud)",
		"💾 Data added:",
		"  - nas-2026: +3.3 GiB (top: immich +2.8 GiB)",
		"  - offsite: +410.0 MiB (top: web +410.0 MiB)",
	}
	if m.Title != "✅ StackBelay · nas01 · 18–25 Nov" || m.Body != strings.Join(wantLines, "\n") || m.Priority != PriorityDefault {
		t.Errorf("message %q\n%s", m.Title, m.Body)
	}
}

func TestSummaryQuietPeriodAndLimit(t *testing.T) {
	// Nothing happened: the summary still goes out (heartbeat).
	s := BuildSummary(SummaryInput{From: periodFrom, To: periodFrom.Add(24 * time.Hour), StaleAfter: 48 * time.Hour,
		Stacks: []StackFacts{healthy("web")}})
	m := FormatSummary("nas01", s, time.UTC)
	if m.Title != "✅ StackBelay · nas01 · 18–19 Nov" || m.Body != "All 1 stacks OK.\n\n📦 Backups: 0 ok · 0 failed\n⏱ Downtime: none" {
		t.Errorf("quiet: %q\n%s", m.Title, m.Body)
	}

	// Very many updates: the attention section stays whole, the rest is cut.
	s = Summary{From: periodFrom, To: periodTo, Attention: []string{"web: stopped"}, UpdatesApplied: 300}
	for i := range 300 {
		s.UpdateLines = append(s.UpdateLines, fmt.Sprintf("stack-with-a-long-name-%03d: app, db, worker", i))
	}
	m = FormatSummary("nas01", s, time.UTC)
	if len(m.Body) > maxBodyBytes || !strings.HasPrefix(m.Body, "1 item needs attention:\n- web: stopped\n") ||
		!strings.HasSuffix(m.Body, " more") || !strings.HasPrefix(m.Title, "⚠️") {
		t.Errorf("limit: %d bytes, %q … %q", len(m.Body), m.Body[:60], m.Body[len(m.Body)-30:])
	}
}

func TestFormatPeriod(t *testing.T) {
	d := func(m time.Month, day, h int) time.Time { return time.Date(2026, m, day, h, 0, 0, 0, time.UTC) }
	tests := []struct {
		from, to time.Time
		want     string
	}{
		{d(11, 18, 7), d(11, 25, 7), "18–25 Nov"},
		{d(11, 28, 7), d(12, 2, 7), "28 Nov – 2 Dec"},
		{d(11, 25, 7), d(11, 25, 13), "25 Nov 07:00–13:00"},
	}
	for _, tt := range tests {
		if got := formatPeriod(tt.from, tt.to, time.UTC); got != tt.want {
			t.Errorf("got %q, want %q", got, tt.want)
		}
	}
}

// fakeSummaryStore implements SummaryStore with fixed data.
type fakeSummaryStore struct {
	settings store.NotifySettings
	sentAt   time.Time
	stacks   []store.StackSummary
	jobs     []store.Job
}

func (f *fakeSummaryStore) NotifySettings(context.Context) (store.NotifySettings, error) {
	return f.settings, nil
}
func (f *fakeSummaryStore) SummarySentAt(context.Context) (time.Time, error) { return f.sentAt, nil }
func (f *fakeSummaryStore) SetSummarySentAt(_ context.Context, at time.Time) error {
	f.sentAt = at
	return nil
}
func (f *fakeSummaryStore) StackSummaries(context.Context) ([]store.StackSummary, error) {
	return f.stacks, nil
}
func (f *fakeSummaryStore) MutedStacks(context.Context) (map[int64]bool, error) { return nil, nil }
func (f *fakeSummaryStore) StacksWithBackupSchedule(context.Context) (map[int64]bool, error) {
	return map[int64]bool{1: true}, nil
}
func (f *fakeSummaryStore) StoppedOnPurpose(context.Context) (map[int64]bool, error) { return nil, nil }
func (f *fakeSummaryStore) JobsInState(context.Context, jobs.State) ([]store.Job, error) {
	return nil, nil
}
func (f *fakeSummaryStore) JobsFinishedBetween(_ context.Context, from, to time.Time) ([]store.Job, error) {
	return f.jobs, nil
}
func (f *fakeSummaryStore) AuditBetween(context.Context, time.Time, time.Time) ([]store.AuditEntry, error) {
	return nil, nil
}
func (f *fakeSummaryStore) DataAddedBetween(context.Context, time.Time, time.Time, string) ([]store.AddedData, error) {
	return nil, nil
}

func TestSummarizerSchedule(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 25, 6, 0, 0, 0, time.UTC)
	st := &fakeSummaryStore{
		settings: store.NotifySettings{ServerURL: "https://ntfy.sh", Topic: "t", SummaryCron: "0 7 * * *", StaleDays: 2},
		stacks: []store.StackSummary{{Stack: store.Stack{ID: 1, Name: "web", Present: true}, HasRepository: true,
			LastFinal: store.FinalBackup{JobBrief: store.JobBrief{Time: now.Add(-time.Hour)}}}},
	}
	snd := &fakeSender{}
	s := &Summarizer{Store: st, Sender: snd, Hostname: "nas01.lan", Logger: slog.New(slog.DiscardHandler),
		Location: time.UTC, Now: func() time.Time { return now }}

	// First run: only starts the period.
	s.Step(ctx)
	if len(snd.sent) != 0 || !st.sentAt.Equal(now) {
		t.Fatalf("first run: sent %d, period from %v", len(snd.sent), st.sentAt)
	}
	now = now.Add(59 * time.Minute) // 06:59
	s.Step(ctx)
	if len(snd.sent) != 0 {
		t.Fatal("not due yet")
	}
	now = now.Add(time.Minute) // 07:00
	s.Step(ctx)
	if len(snd.sent) != 1 || !st.sentAt.Equal(now) || snd.sent[0].Title != "✅ StackBelay · nas01 · 25 Nov 06:00–07:00" {
		t.Fatalf("due: %+v, sent at %v", snd.sent, st.sentAt)
	}

	// The manager was down for three days: one summary covers the gap.
	now = now.Add(3*24*time.Hour + 2*time.Hour)
	s.Step(ctx)
	// The fake's last backup is from before the gap: now stale.
	if len(snd.sent) != 2 || snd.sent[1].Title != "⚠️ StackBelay · nas01 · 25–28 Nov" ||
		!strings.Contains(snd.sent[1].Body, "web: no backup for 3 d") {
		t.Fatalf("catch-up: %+v", snd.sent)
	}

	// Sending fails: retried after the backoff, the period is kept.
	now = now.Add(24 * time.Hour)
	snd.err = errors.New("down")
	sentAt := st.sentAt
	s.Step(ctx)
	if !st.sentAt.Equal(sentAt) || s.failures != 1 {
		t.Fatal("a failed send must keep the period")
	}
	snd.err = nil
	s.Step(ctx)
	if len(snd.sent) != 2 {
		t.Fatal("must wait for the backoff")
	}
	now = now.Add(time.Minute)
	s.Step(ctx)
	if len(snd.sent) != 3 {
		t.Fatal("retry after the backoff")
	}

	// Turned off: nothing is sent.
	st.settings.Events = map[string]bool{string(KeySummary): false}
	now = now.Add(48 * time.Hour)
	s.Step(ctx)
	if len(snd.sent) != 3 {
		t.Fatal("summary off")
	}
}
