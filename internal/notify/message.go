package notify

import (
	"fmt"
	"strings"
	"time"

	"github.com/MB512/stackbelay/internal/jobs"
	"github.com/MB512/stackbelay/internal/store"
)

// Message is one push notification.
type Message struct {
	Title    string
	Body     string
	Priority int
}

// maxBodyBytes keeps a message below ntfy's limit of 4096 bytes (longer
// messages become attachments, which a phone does not show inline).
const maxBodyBytes = 3900

// Event is a finished job, or a finding of the stack monitor, with its
// event type.
type Event struct {
	Key Key
	Job store.Job // a job event; zero for a stack event
	// Stack events (issue #49): the stack ("" = the whole host) and what
	// the monitor saw.
	Stack  string
	Detail StackDetail
}

// isStack reports whether ev is an event of the stack monitor.
func (ev Event) isStack() bool { return ev.Job.Type == "" }

// subject is the stack the event is about, or "host".
func (ev Event) subject() string {
	if ev.isStack() {
		if ev.Stack == "" {
			return "host"
		}
		return ev.Stack
	}
	return subject(ev.Job)
}

// ShortHost returns host up to its first dot: messages name the host, not
// its domain (issue #47).
func ShortHost(host string) string {
	short, _, _ := strings.Cut(host, ".")
	return short
}

// Format builds the message for events (at least one): a detailed one for
// a single event, a combined one with a line per event otherwise. Times
// are shown in loc. Messages contain no links, user names, image tags or
// raw errors (see docs/security.md).
func Format(host string, events []Event, loc *time.Location) Message {
	if len(events) == 1 {
		return formatOne(host, events[0], loc)
	}
	return formatMany(host, events)
}

func formatOne(host string, ev Event, loc *time.Location) Message {
	if ev.isStack() {
		return formatStackEvent(host, ev)
	}
	info, _ := Info(ev.Key)
	j := ev.Job
	title := fmt.Sprintf("%s %s · %s", info.Emoji, eventLabel(ev), subject(j))

	trigger := "manual"
	if j.Trigger == store.TriggerSchedule {
		trigger = "scheduled"
	}
	lines := []string{strings.Join(nonEmpty(host, trigger, timeRange(j, loc)), " · ")}
	if s := stateLine(ev); s != "" {
		lines = append(lines, s)
	}
	if j.State == jobs.StateFailed {
		lines = append(lines, "Reason: "+Reason(j.Error))
	}
	return Message{Title: title, Body: limitBody(lines), Priority: info.Priority}
}

func formatMany(host string, events []Event) Message {
	prio, sameKey := 0, true
	for _, ev := range events {
		info, _ := Info(ev.Key)
		prio = max(prio, info.Priority)
		sameKey = sameKey && ev.Key == events[0].Key
	}
	var title string
	if first, _ := Info(events[0].Key); sameKey {
		title = fmt.Sprintf("%s %d× %s · %s", first.Emoji, len(events), first.Label, host)
	} else {
		title = fmt.Sprintf("%s %d events · %s", emojiOf(prio), len(events), host)
	}
	lines := make([]string, 0, len(events))
	for _, ev := range events {
		info, _ := Info(ev.Key)
		line := fmt.Sprintf("%s %s: %s", info.Emoji, ev.subject(), lowerFirst(eventLabel(ev)))
		if detail := shortDetail(ev); detail != "" {
			line += " (" + detail + ")"
		}
		lines = append(lines, line)
	}
	return Message{Title: title, Body: limitBody(lines), Priority: prio}
}

// eventLabel names what happened, more precisely than the event type's
// label where the job type is known.
func eventLabel(ev Event) string {
	j := ev.Job
	switch ev.Key {
	case KeyRollback:
		if j.Type == jobs.TypeComposeApply {
			return "Compose change rolled back"
		}
		return "Update rolled back"
	case KeyLeftDown:
		return typeName(j.Type) + " failed"
	case KeyOperationFailed:
		return typeName(j.Type) + " failed"
	case KeyOperationSucceeded:
		return typeName(j.Type) + " done"
	}
	info, _ := Info(ev.Key)
	return info.Label
}

// typeName is the job type as a word at the start of a label.
func typeName(t jobs.Type) string {
	switch t {
	case jobs.TypeComposeApply:
		return "Compose change"
	case jobs.TypeUp:
		return "Up"
	case jobs.TypeDown:
		return "Down"
	case jobs.TypeCleanup:
		return "Host cleanup"
	}
	s := string(t)
	if s == "" {
		return "Job"
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// subject is the stack, or "host" for a host job.
func subject(j store.Job) string {
	if j.StackName == "" {
		return "host"
	}
	return j.StackName
}

// stateLine says what state the job left the stack in (detailed message).
func stateLine(ev Event) string {
	j := ev.Job
	switch {
	case ev.Key == KeyLeftDown:
		return "Stack is DOWN. Not running: " + strings.Join(j.Result.ServicesDown, ", ")
	case ev.Key == KeyRollback:
		return "Stack runs the previous version."
	case j.State == jobs.StateFailed && restarts(j) && Reason(j.Error) != "worker lost":
		// The worker found no service down that ran before the job.
		return "Services that ran before are running."
	case j.State == jobs.StateSucceeded && len(j.Result.ImagesChanged) > 0:
		return "Changed: " + strings.Join(j.Result.ImagesChanged, ", ")
	case j.State == jobs.StateSucceeded && j.Type == jobs.TypeCleanup:
		return fmt.Sprintf("Removed %d images and %d containers, freed %s.",
			j.Result.RemovedImages, j.Result.RemovedContainers, formatBytes(j.Result.FreedBytes))
	case j.State == jobs.StateSucceeded && j.Type == jobs.TypeRestore && j.Options.NoStart:
		return "Stack was not started (as requested)."
	}
	return ""
}

// shortDetail is the part in parentheses of a line of a combined message.
func shortDetail(ev Event) string {
	if ev.isStack() {
		return ev.Detail.short(ev.Key)
	}
	j := ev.Job
	switch {
	case ev.Key == KeyLeftDown:
		return "DOWN: " + strings.Join(j.Result.ServicesDown, ", ")
	case j.State == jobs.StateFailed:
		return Reason(j.Error)
	case len(j.Result.ImagesChanged) > 0:
		return strings.Join(j.Result.ImagesChanged, ", ")
	}
	return ""
}

// restarts reports whether the job type stops a running stack and starts
// it again (the worker checks those for services left down).
func restarts(j store.Job) bool {
	switch j.Type {
	case jobs.TypeBackup, jobs.TypeUpdate, jobs.TypeComposeApply:
		return true
	case jobs.TypeRestore:
		return !j.Options.NoStart
	}
	return false
}

// timeRange is "03:00–03:14" in loc; with the date if the job did not
// start and finish on the same day.
func timeRange(j store.Job, loc *time.Location) string {
	if j.FinishedAt.IsZero() {
		return ""
	}
	end := j.FinishedAt.In(loc)
	if j.StartedAt.IsZero() {
		return end.Format("2 Jan 15:04")
	}
	start := j.StartedAt.In(loc)
	if start.YearDay() != end.YearDay() || start.Year() != end.Year() {
		return start.Format("2 Jan 15:04") + "–" + end.Format("2 Jan 15:04")
	}
	return start.Format("2 Jan 15:04") + "–" + end.Format("15:04")
}

func emojiOf(priority int) string {
	switch {
	case priority >= PriorityUrgent:
		return "🚨"
	case priority >= PriorityHigh:
		return "⚠️"
	}
	return "ℹ️"
}

// limitBody joins lines, cut to maxBodyBytes: lines that do not fit are
// replaced by "… and N more".
func limitBody(lines []string) string { return limitBodyTo(lines, maxBodyBytes) }

// limitBodyTo is limitBody with another limit.
func limitBodyTo(lines []string, limit int) string {
	const markerRoom = 32 // enough for "… and N more"
	var b strings.Builder
	for i, line := range lines {
		if b.Len()+len(line)+markerRoom > limit {
			fmt.Fprintf(&b, "… and %d more", len(lines)-i)
			return b.String()
		}
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(line)
	}
	return b.String()
}

func nonEmpty(parts ...string) []string {
	out := parts[:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// formatBytes formats n with binary units ("1.2 GiB").
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// lowerFirst lowercases the first letter of a label ("Backup failed" ->
// "backup failed"), keeping names like StackBelay.
func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}
