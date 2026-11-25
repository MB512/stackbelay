package notify

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/MB512/stackbelay/internal/docker"
	"github.com/MB512/stackbelay/internal/jobs"
	"github.com/MB512/stackbelay/internal/store"
)

// The periodic summary (issue #48): what needs attention now, then what
// happened since the last summary.

// StackFacts is the state of one stack when the summary is built.
type StackFacts struct {
	Name              string
	Muted             bool
	HasRepository     bool
	HasBackupSchedule bool
	LastBackup        store.JobBrief // newest backup job; zero if none
	LastGood          time.Time      // newest consistent snapshot; zero if none
	Busy              bool           // a job of the stack runs
	// Condition is the stack's state from docker; "" if unknown.
	Condition docker.Condition
	Problems  []string // of a degraded stack
	// StoppedOnPurpose: the newest stop/start job stopped it.
	StoppedOnPurpose  bool
	RestoreIncomplete bool
	// UpdatesAvailable counts running services with a newer image.
	UpdatesAvailable int
}

// SummaryInput is everything a summary is built from.
type SummaryInput struct {
	From, To   time.Time
	Stacks     []StackFacts // present stacks
	Jobs       []store.Job  // finished in the period
	Audit      []store.AuditEntry
	Added      []store.AddedData
	StaleAfter time.Duration
}

// Summary holds the sections of a summary message.
type Summary struct {
	From, To  time.Time
	Stacks    int      // stacks covered (present, not muted)
	Attention []string // lines, most important first

	BackupsOK, BackupsFailed, BackupsCancelled int

	Downtime        time.Duration
	Longest         time.Duration
	LongestStack    string
	LongestType     jobs.Type
	LongestFinished time.Time

	UpdatesApplied, UpdatesUnchanged, UpdatesFailed, UpdatesRolledBack int
	UpdateLines                                                        []string // "immich: server, machine-learning"

	ComposeEdits    []string       // stacks, "gitea (rolled back)"
	SettingsChanges map[string]int // "schedules" -> 2

	Restores []string // stacks, "paperless (failed)"

	FailedLogins, UserChanges, ShellSessions int
	ShellStacks                              []string

	Added []RepoAdded
}

// RepoAdded is the data added to one repository in the period.
type RepoAdded struct {
	Repository string
	Bytes      uint64
	TopStack   string
	TopBytes   uint64
}

// settingsGroups maps audit actions to the groups of the "settings
// changes" line. User and permission changes are counted under security.
var settingsGroups = map[string]string{
	"schedule.create": "schedules", "schedule.delete": "schedules", "schedule.update": "schedules",
	"repository.create": "repositories", "repository.update": "repositories",
	"repository.delete": "repositories", "repository.init": "repositories",
	"stack.repository": "stack settings", "stack.source_host": "stack settings",
	"stack.notify_muted": "stack settings", "stack.forget": "stack settings",
	"notify.update": "notifications",
}

// BuildSummary computes the summary of a period.
func BuildSummary(in SummaryInput) Summary {
	s := Summary{From: in.From, To: in.To, SettingsChanges: map[string]int{}}
	s.Attention = attention(in, &s)
	countJobs(in.Jobs, &s)
	countAudit(in.Audit, &s)
	s.Added = addedPerRepo(in.Added)
	return s
}

// attention lists what needs attention now, per stack: an incomplete
// restore, a failed or stale backup, a missing repository or schedule, a
// degraded or unexpectedly stopped stack; then the stacks with updates.
// Muted stacks are left out.
func attention(in SummaryInput, s *Summary) []string {
	var lines, updates []string
	for _, st := range in.Stacks {
		if st.Muted {
			continue
		}
		s.Stacks++
		add := func(format string, args ...any) {
			lines = append(lines, st.Name+": "+fmt.Sprintf(format, args...))
		}
		if st.RestoreIncomplete {
			add("restore incomplete")
		}
		switch {
		case !st.HasRepository:
			add("no repository")
		case st.LastBackup.State == jobs.StateFailed:
			add("last backup failed, %s", lastGood(st.LastGood, in.To))
		case !st.HasBackupSchedule:
			add("no backup schedule")
		case st.LastGood.IsZero():
			add("never backed up")
		case in.To.Sub(st.LastGood) > in.StaleAfter:
			add("no backup for %s", formatAge(in.To.Sub(st.LastGood)))
		}
		if !st.Busy {
			switch {
			case st.Condition == docker.ConditionDegraded:
				add("degraded (%s)", strings.Join(st.Problems, "; "))
			case st.Condition == docker.ConditionStopped && !st.StoppedOnPurpose:
				add("stopped")
			}
		}
		if st.UpdatesAvailable > 0 {
			updates = append(updates, st.Name)
		}
	}
	if len(updates) > 0 {
		lines = append(lines, "updates available: "+strings.Join(updates, ", "))
	}
	return lines
}

func lastGood(t, now time.Time) string {
	if t.IsZero() {
		return "never succeeded"
	}
	return "last good " + formatAge(now.Sub(t)) + " ago"
}

// countJobs fills the job sections.
func countJobs(list []store.Job, s *Summary) {
	for _, j := range list {
		switch j.Type {
		case jobs.TypeBackup:
			switch j.State {
			case jobs.StateSucceeded:
				s.BackupsOK++
			case jobs.StateFailed:
				s.BackupsFailed++
			case jobs.StateCancelled:
				s.BackupsCancelled++
			}
		case jobs.TypeUpdate:
			switch {
			case j.State == jobs.StateSucceeded && j.Result.Unchanged:
				s.UpdatesUnchanged++
			case j.State == jobs.StateSucceeded:
				s.UpdatesApplied++
				line := j.StackName
				if len(j.Result.ImagesChanged) > 0 {
					line += ": " + strings.Join(j.Result.ImagesChanged, ", ")
				}
				s.UpdateLines = append(s.UpdateLines, line)
			case j.State == jobs.StateFailed && j.Result.RolledBack:
				s.UpdatesRolledBack++
				s.UpdateLines = append(s.UpdateLines, j.StackName+": rolled back")
			case j.State == jobs.StateFailed:
				s.UpdatesFailed++
				s.UpdateLines = append(s.UpdateLines, j.StackName+": failed")
			}
		case jobs.TypeComposeApply:
			switch {
			case j.State == jobs.StateSucceeded:
				s.ComposeEdits = append(s.ComposeEdits, j.StackName)
			case j.State == jobs.StateFailed && j.Result.RolledBack:
				s.ComposeEdits = append(s.ComposeEdits, j.StackName+" (rolled back)")
			}
		case jobs.TypeRestore:
			switch j.State {
			case jobs.StateSucceeded:
				s.Restores = append(s.Restores, j.StackName)
			case jobs.StateFailed:
				s.Restores = append(s.Restores, j.StackName+" (failed)")
			}
		}
		countDowntime(j, s)
	}
}

// countDowntime adds the downtime a backup, update or compose change
// caused (stop until start, or until the job finished if it did not start
// the stack again). User stops and restores do not count (issue #48).
func countDowntime(j store.Job, s *Summary) {
	switch j.Type {
	case jobs.TypeBackup, jobs.TypeUpdate, jobs.TypeComposeApply:
	default:
		return
	}
	if j.Result.StackStoppedAt == nil {
		return
	}
	end := j.FinishedAt
	if j.Result.StackStartedAt != nil {
		end = *j.Result.StackStartedAt
	}
	d := end.Sub(*j.Result.StackStoppedAt)
	if d <= 0 {
		return
	}
	s.Downtime += d
	if d > s.Longest {
		s.Longest, s.LongestStack, s.LongestType, s.LongestFinished = d, j.StackName, j.Type, j.FinishedAt
	}
}

// countAudit fills the settings and security counts.
func countAudit(entries []store.AuditEntry, s *Summary) {
	shells := map[string]bool{}
	for _, e := range entries {
		switch e.Action {
		case "login.failure", "password.change_failed":
			s.FailedLogins++
		case "user.create", "user.update", "user.password_reset", "permission.update":
			s.UserChanges++
		case "shell.start":
			s.ShellSessions++
			if e.StackName != "" {
				shells[e.StackName] = true
			}
		default:
			if g, ok := settingsGroups[e.Action]; ok {
				s.SettingsChanges[g]++
			}
		}
	}
	s.ShellStacks = slices.Sorted(maps.Keys(shells))
}

// addedPerRepo sums the added data per repository, largest first.
func addedPerRepo(list []store.AddedData) []RepoAdded {
	perStack := map[string]map[string]uint64{} // repository -> stack -> bytes
	for _, a := range list {
		if perStack[a.Repository] == nil {
			perStack[a.Repository] = map[string]uint64{}
		}
		perStack[a.Repository][a.Stack] += a.Bytes
	}
	var out []RepoAdded
	for repo, stacks := range perStack {
		r := RepoAdded{Repository: repo}
		for _, name := range slices.Sorted(maps.Keys(stacks)) {
			b := stacks[name]
			r.Bytes += b
			if b > r.TopBytes {
				r.TopStack, r.TopBytes = name, b
			}
		}
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b RepoAdded) int {
		return cmp.Or(cmp.Compare(b.Bytes, a.Bytes), cmp.Compare(a.Repository, b.Repository))
	})
	return out
}

// FormatSummary builds the summary message. The attention section is never
// cut; the others are, below ntfy's size limit.
func FormatSummary(host string, s Summary, loc *time.Location) Message {
	emoji, status := "✅", fmt.Sprintf("All %d stacks OK.", s.Stacks)
	if n := len(s.Attention); n > 0 {
		emoji = "⚠️"
		status = fmt.Sprintf("%d items need attention:", n)
		if n == 1 {
			status = "1 item needs attention:"
		}
	}
	head := []string{status}
	for _, a := range s.Attention {
		head = append(head, "- "+a)
	}

	backups := fmt.Sprintf("📦 Backups: %d ok · %d failed", s.BackupsOK, s.BackupsFailed)
	if s.BackupsCancelled > 0 {
		backups += fmt.Sprintf(" · %d cancelled", s.BackupsCancelled)
	}
	rest := []string{"", backups}
	if s.Downtime > 0 {
		rest = append(rest, fmt.Sprintf("⏱ Downtime: total %s · longest: %s %s (%s, %s)", formatDuration(s.Downtime),
			s.LongestStack, formatDuration(s.Longest), typeWord(s.LongestType), s.LongestFinished.In(loc).Format("Mon")))
	} else {
		rest = append(rest, "⏱ Downtime: none")
	}
	if n := s.UpdatesApplied + s.UpdatesUnchanged + s.UpdatesFailed + s.UpdatesRolledBack; n > 0 {
		parts := []string{fmt.Sprintf("%d applied", s.UpdatesApplied), fmt.Sprintf("%d unchanged", s.UpdatesUnchanged)}
		if s.UpdatesFailed > 0 {
			parts = append(parts, fmt.Sprintf("%d failed", s.UpdatesFailed))
		}
		if s.UpdatesRolledBack > 0 {
			parts = append(parts, fmt.Sprintf("%d rolled back", s.UpdatesRolledBack))
		}
		rest = append(rest, "🔄 Updates: "+strings.Join(parts, " · "))
		for _, l := range s.UpdateLines {
			rest = append(rest, "  - "+l)
		}
	}
	var changes []string
	if n := len(s.ComposeEdits); n > 0 {
		changes = append(changes, fmt.Sprintf("%d compose edit%s (%s)", n, plural(n), strings.Join(s.ComposeEdits, ", ")))
	}
	if len(s.SettingsChanges) > 0 {
		var parts []string
		for _, g := range slices.Sorted(maps.Keys(s.SettingsChanges)) {
			parts = append(parts, fmt.Sprintf("%d %s", s.SettingsChanges[g], g))
		}
		changes = append(changes, "settings: "+strings.Join(parts, ", "))
	}
	if len(changes) > 0 {
		rest = append(rest, "🛠 Changes: "+strings.Join(changes, " · "))
	}
	if n := len(s.Restores); n > 0 {
		rest = append(rest, fmt.Sprintf("♻️ Restores: %d (%s)", n, strings.Join(s.Restores, ", ")))
	}
	var sec []string
	if s.FailedLogins > 0 {
		sec = append(sec, fmt.Sprintf("%d failed login%s", s.FailedLogins, plural(s.FailedLogins)))
	}
	if s.UserChanges > 0 {
		sec = append(sec, fmt.Sprintf("%d user/permission change%s", s.UserChanges, plural(s.UserChanges)))
	}
	if s.ShellSessions > 0 {
		shell := fmt.Sprintf("%d shell session%s", s.ShellSessions, plural(s.ShellSessions))
		if len(s.ShellStacks) > 0 {
			shell += " (" + strings.Join(s.ShellStacks, ", ") + ")"
		}
		sec = append(sec, shell)
	}
	if len(sec) > 0 {
		rest = append(rest, "🔐 Security: "+strings.Join(sec, " · "))
	}
	if len(s.Added) > 0 {
		rest = append(rest, "💾 Data added:")
		for _, a := range s.Added {
			line := fmt.Sprintf("  - %s: +%s", a.Repository, formatBytes(int64(a.Bytes))) //nolint:gosec // G115: repository sizes fit
			if a.TopStack != "" {
				line += fmt.Sprintf(" (top: %s +%s)", a.TopStack, formatBytes(int64(a.TopBytes))) //nolint:gosec // G115: see above
			}
			rest = append(rest, line)
		}
	}

	headText := strings.Join(head, "\n")
	body := headText
	if budget := maxBodyBytes - len(headText) - 1; budget > 64 {
		body += "\n" + limitBodyTo(rest, budget)
	}
	title := fmt.Sprintf("%s StackBelay · %s · %s", emoji, host, formatPeriod(s.From, s.To, loc))
	return Message{Title: title, Body: body, Priority: PriorityDefault}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func typeWord(t jobs.Type) string {
	if t == jobs.TypeComposeApply {
		return "compose change"
	}
	return string(t)
}

// formatPeriod is "18–25 Nov", "28 Nov – 2 Dec", or "25 Nov 07:00–13:00"
// for a period within one day.
func formatPeriod(from, to time.Time, loc *time.Location) string {
	f, t := from.In(loc), to.In(loc)
	switch {
	case f.YearDay() == t.YearDay() && f.Year() == t.Year():
		return f.Format("2 Jan 15:04") + "–" + t.Format("15:04")
	case f.Month() == t.Month() && f.Year() == t.Year():
		return f.Format("2") + "–" + t.Format("2 Jan")
	}
	return f.Format("2 Jan") + " – " + t.Format("2 Jan")
}

// formatDuration is "45 s", "14 min", "1 h 42 min".
func formatDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	}
	h := int(d.Hours())
	return fmt.Sprintf("%d h %d min", h, int(d.Minutes())-60*h)
}

// formatAge is "5 h", "3 d".
func formatAge(d time.Duration) string {
	if d < 48*time.Hour {
		return fmt.Sprintf("%d h", int(d.Hours()))
	}
	return fmt.Sprintf("%d d", int(d.Hours()/24))
}
