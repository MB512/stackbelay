package notify

import (
	"context"
	"log/slog"
	"time"

	"github.com/MB512/stackbelay/internal/store"
)

// Store is what the notifier needs from the database (*store.Store).
type Store interface {
	NotifySettings(ctx context.Context) (store.NotifySettings, error)
	NotifyEvents(ctx context.Context) ([]store.NotifyEvent, error)
	DeleteNotifyEvents(ctx context.Context, ids []int64) error
	MutedStacks(ctx context.Context) (map[int64]bool, error)
}

// Sender delivers a message (*NTFY; a fake in tests).
type Sender interface {
	Send(ctx context.Context, t Target, m Message) error
}

// Timing of the notifier (issue #47).
const (
	// window: events are sent once no new one arrived for this long ...
	window = time.Minute
	// maxWait: ... or the oldest waited this long.
	maxWait = 5 * time.Minute
	// giveUp: events this old are dropped (ntfy unreachable for so long).
	giveUp = 24 * time.Hour
	// retry backoff after a failed send: doubles from minBackoff up to
	// maxBackoff.
	minBackoff = time.Minute
	maxBackoff = 30 * time.Minute
	// pollInterval: how often the outbox is checked.
	pollInterval = 10 * time.Second
)

// Notifier sends the outbox (store.NotifyEvent rows) to ntfy. It runs in
// the manager as one goroutine; all its state besides the retry backoff is
// in the database, so a restart loses nothing.
type Notifier struct {
	Store  Store
	Sender Sender
	// Host names this host in messages (STACKBELAY_HOSTNAME, shortened).
	Host   string
	Logger *slog.Logger
	// Location is the time zone of the times in messages; default time.Local
	// (the manager's TZ).
	Location *time.Location
	// Now defaults to time.Now.
	Now func() time.Time

	failures    int       // failed sends in a row
	nextAttempt time.Time // no send before this time (backoff)
}

func (n *Notifier) now() time.Time {
	if n.Now == nil {
		return time.Now()
	}
	return n.Now()
}

// Run checks the outbox until ctx is cancelled.
func (n *Notifier) Run(ctx context.Context) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		n.Step(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Step looks at the outbox once: it drops events that do not become a
// message (notifications off, event type off, stack muted, too old) and
// sends the rest as one message once the window has passed. Run calls it
// in a loop; tests call it directly.
func (n *Notifier) Step(ctx context.Context) {
	events, err := n.Store.NotifyEvents(ctx)
	if err != nil {
		n.Logger.Error("notifications: reading events", "error", err)
		return
	}
	if len(events) == 0 {
		return
	}
	settings, err := n.Store.NotifySettings(ctx)
	if err != nil {
		n.Logger.Error("notifications: reading settings", "error", err)
		return
	}
	muted, err := n.Store.MutedStacks(ctx)
	if err != nil {
		n.Logger.Error("notifications: reading muted stacks", "error", err)
		return
	}
	target, on := TargetOf(settings)
	now := n.now()

	var drop, send []int64
	var batch []Event
	var oldest, newest time.Time
	for _, ev := range events {
		key, ok := Classify(ev.Job)
		switch {
		case !on || !ok || !Enabled(settings.Events, key) || muted[ev.Job.StackID]:
			drop = append(drop, ev.ID)
		case now.Sub(ev.CreatedAt) > giveUp:
			n.Logger.Warn("notifications: dropping an event that could not be sent for too long",
				"job_id", ev.Job.ID, "event", key, "since", ev.CreatedAt)
			drop = append(drop, ev.ID)
		default:
			send = append(send, ev.ID)
			batch = append(batch, Event{Key: key, Job: ev.Job})
			if oldest.IsZero() || ev.CreatedAt.Before(oldest) {
				oldest = ev.CreatedAt
			}
			if ev.CreatedAt.After(newest) {
				newest = ev.CreatedAt
			}
		}
	}
	n.delete(ctx, drop)
	if len(batch) == 0 {
		n.failures = 0
		return
	}
	if now.Sub(newest) < window && now.Sub(oldest) < maxWait {
		return // more events may follow (e.g. several stacks scheduled at the same time)
	}
	if now.Before(n.nextAttempt) {
		return
	}

	loc := n.Location
	if loc == nil {
		loc = time.Local
	}
	msg := Format(n.Host, batch, loc)
	sctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if err := n.Sender.Send(sctx, target, msg); err != nil {
		n.failures++
		n.Logger.Warn("notifications: sending failed, retrying later", "events", len(batch),
			"retry_in", n.backoff().String(), "error", err)
		n.nextAttempt = now.Add(n.backoff())
		return
	}
	n.failures, n.nextAttempt = 0, time.Time{}
	n.Logger.Info("notification sent", "events", len(batch), "title", msg.Title)
	n.delete(ctx, send)
}

// backoff is the wait after the notifier's failed sends in a row.
func (n *Notifier) backoff() time.Duration { return backoffAfter(n.failures) }

// backoffAfter is the wait after failures failed sends in a row: doubling
// from minBackoff, capped at maxBackoff (checked before shifting, so many
// failures cannot overflow).
func backoffAfter(failures int) time.Duration {
	if failures > 10 {
		return maxBackoff
	}
	return min(minBackoff<<(max(failures, 1)-1), maxBackoff)
}

func (n *Notifier) delete(ctx context.Context, ids []int64) {
	if len(ids) == 0 {
		return
	}
	if err := n.Store.DeleteNotifyEvents(ctx, ids); err != nil {
		n.Logger.Error("notifications: deleting events", "error", err)
	}
}
