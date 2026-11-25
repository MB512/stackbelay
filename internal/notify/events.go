// Package notify sends push notifications through ntfy (issue #47): one
// message per finished job, or one combined message when several finish
// within a short window.
package notify

import (
	"github.com/MB512/stackbelay/internal/jobs"
	"github.com/MB512/stackbelay/internal/store"
)

// Key identifies an event type. Admins turn each one on or off; the keys
// are stored in notify_settings.events, so they must never change.
type Key string

// Event keys. The job types form a matrix with the outcomes failed and
// succeeded; rollback and left-down are separate rows.
const (
	KeyBackupFailed       Key = "backup.failed"
	KeyBackupSucceeded    Key = "backup.succeeded"
	KeyUpdateFailed       Key = "update.failed"
	KeyUpdateSucceeded    Key = "update.succeeded" // an update that changed something
	KeyComposeFailed      Key = "compose.failed"
	KeyComposeSucceeded   Key = "compose.succeeded"
	KeyRestoreFailed      Key = "restore.failed"
	KeyRestoreSucceeded   Key = "restore.succeeded"
	KeyOperationFailed    Key = "operation.failed" // stop, start, restart, up, down
	KeyOperationSucceeded Key = "operation.succeeded"
	KeyCleanupFailed      Key = "cleanup.failed"
	KeyCleanupSucceeded   Key = "cleanup.succeeded"
	// KeyRollback: an update or compose change failed and was rolled back.
	KeyRollback Key = "rollback"
	// KeyLeftDown: a failed job left services down that ran before it.
	KeyLeftDown Key = "left_down"
	// KeySummary: the periodic summary (issue #48).
	KeySummary Key = "summary"
	// Stack monitor (issue #49): a stack's state changed outside a job.
	KeyStackDegraded   Key = "stack.degraded"
	KeyStackStopped    Key = "stack.stopped" // stopped outside StackBelay
	KeyStackRecovered  Key = "stack.recovered"
	KeyStackUnreadable Key = "stack.unreadable" // also for the whole host (docker unreachable)
	KeyStackMissing    Key = "stack.missing"    // the stack directory vanished
)

// ntfy priorities (1 = min ... 5 = urgent). Urgent breaks through Do Not
// Disturb on Android, so only a stack left down uses it.
const (
	PriorityLow     = 2
	PriorityDefault = 3
	PriorityHigh    = 4
	PriorityUrgent  = 5
)

// EventInfo describes an event type for the settings page and the
// messages.
type EventInfo struct {
	Key      Key
	Label    string // "Backup failed"
	Default  bool   // on unless the admin turned it off
	Priority int
	Emoji    string
}

// Events lists every event type, in the order of the settings page.
var Events = []EventInfo{
	{KeyBackupFailed, "Backup failed", true, PriorityHigh, "❌"},
	{KeyBackupSucceeded, "Backup succeeded", false, PriorityLow, "✅"},
	{KeyUpdateFailed, "Update failed", true, PriorityHigh, "❌"},
	{KeyUpdateSucceeded, "Update applied", false, PriorityLow, "✅"},
	{KeyComposeFailed, "Compose change failed", true, PriorityHigh, "❌"},
	{KeyComposeSucceeded, "Compose change applied", false, PriorityLow, "✅"},
	{KeyRestoreFailed, "Restore failed", true, PriorityHigh, "❌"},
	{KeyRestoreSucceeded, "Restore succeeded", false, PriorityLow, "✅"},
	{KeyOperationFailed, "Stack operation failed", true, PriorityHigh, "❌"},
	{KeyOperationSucceeded, "Stack operation succeeded", false, PriorityLow, "✅"},
	{KeyCleanupFailed, "Host cleanup failed", true, PriorityDefault, "❌"},
	{KeyCleanupSucceeded, "Host cleanup succeeded", false, PriorityLow, "✅"},
	{KeyRollback, "Rolled back", true, PriorityHigh, "↩️"},
	{KeyLeftDown, "Stack left down", true, PriorityUrgent, "🚨"},
	{KeySummary, "Summary", true, PriorityDefault, "📊"},
	{KeyStackDegraded, "Stack degraded", true, PriorityHigh, "⚠️"},
	{KeyStackStopped, "Stopped outside StackBelay", true, PriorityHigh, "⚠️"},
	{KeyStackRecovered, "Stack recovered", true, PriorityDefault, "✅"},
	{KeyStackUnreadable, "State unreadable", true, PriorityDefault, "❔"},
	{KeyStackMissing, "Directory missing", true, PriorityHigh, "⚠️"},
}

// Info returns the description of key; ok is false for an unknown key.
func Info(key Key) (EventInfo, bool) {
	for _, e := range Events {
		if e.Key == key {
			return e, true
		}
	}
	return EventInfo{}, false
}

// Enabled reports whether key is on: the admin's choice, else its default.
func Enabled(settings map[string]bool, key Key) bool {
	if on, ok := settings[string(key)]; ok {
		return on
	}
	info, _ := Info(key)
	return info.Default
}

// Classify returns the event type of a finished job. ok is false for jobs
// that never send a message: cancelled jobs (someone cancelled them on
// purpose) and updates that found nothing to change. Both show up in the
// summary (issue #48).
func Classify(j store.Job) (key Key, ok bool) {
	failed := j.State == jobs.StateFailed
	switch {
	case j.State != jobs.StateFailed && j.State != jobs.StateSucceeded:
		return "", false
	case failed && len(j.Result.ServicesDown) > 0:
		return KeyLeftDown, true
	case failed && j.Result.RolledBack:
		return KeyRollback, true
	}
	pick := func(onFailure, onSuccess Key) (Key, bool) {
		if failed {
			return onFailure, true
		}
		return onSuccess, true
	}
	switch j.Type {
	case jobs.TypeBackup:
		return pick(KeyBackupFailed, KeyBackupSucceeded)
	case jobs.TypeUpdate:
		if !failed && j.Result.Unchanged {
			return "", false
		}
		return pick(KeyUpdateFailed, KeyUpdateSucceeded)
	case jobs.TypeComposeApply:
		return pick(KeyComposeFailed, KeyComposeSucceeded)
	case jobs.TypeRestore:
		return pick(KeyRestoreFailed, KeyRestoreSucceeded)
	case jobs.TypeStop, jobs.TypeStart, jobs.TypeRestart, jobs.TypeUp, jobs.TypeDown:
		return pick(KeyOperationFailed, KeyOperationSucceeded)
	case jobs.TypeCleanup:
		return pick(KeyCleanupFailed, KeyCleanupSucceeded)
	}
	return "", false
}
