package notify

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"testing"
	"time"

	"github.com/MB512/stackbelay/internal/jobs"
	"github.com/MB512/stackbelay/internal/store"
)

type fakeStore struct {
	settings store.NotifySettings
	events   []store.NotifyEvent
	muted    map[int64]bool
}

func (f *fakeStore) NotifySettings(context.Context) (store.NotifySettings, error) {
	return f.settings, nil
}

func (f *fakeStore) NotifyEvents(context.Context) ([]store.NotifyEvent, error) {
	return slices.Clone(f.events), nil
}

func (f *fakeStore) DeleteNotifyEvents(_ context.Context, ids []int64) error {
	f.events = slices.DeleteFunc(f.events, func(e store.NotifyEvent) bool { return slices.Contains(ids, e.ID) })
	return nil
}

func (f *fakeStore) MutedStacks(context.Context) (map[int64]bool, error) { return f.muted, nil }

type fakeSender struct {
	sent []Message
	err  error
}

func (f *fakeSender) Send(_ context.Context, _ Target, m Message) error {
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, m)
	return nil
}

// harness builds a notifier with a clock that tests move.
func harness() (*Notifier, *fakeStore, *fakeSender, *time.Time) {
	now := t0
	st := &fakeStore{settings: store.NotifySettings{ServerURL: "https://ntfy.sh", Topic: "backups"}}
	snd := &fakeSender{}
	n := &Notifier{Store: st, Sender: snd, Host: "nas01", Logger: slog.New(slog.DiscardHandler),
		Location: utc, Now: func() time.Time { return now }}
	return n, st, snd, &now
}

func event(id, stackID int64, state jobs.State, at time.Time) store.NotifyEvent {
	return store.NotifyEvent{ID: id, CreatedAt: at, StackID: stackID, StackName: "web", Job: store.Job{ID: "job", StackID: stackID, StackName: "web",
		Type: jobs.TypeBackup, State: state, Trigger: store.TriggerSchedule, StartedAt: at, FinishedAt: at}}
}

func TestNotifierWindow(t *testing.T) {
	n, st, snd, now := harness()
	ctx := context.Background()
	st.events = []store.NotifyEvent{event(1, 1, jobs.StateFailed, t0)}

	n.Step(ctx)
	if len(snd.sent) != 0 || len(st.events) != 1 {
		t.Fatal("must wait for the window")
	}
	// A second failure within the window extends it.
	*now = t0.Add(50 * time.Second)
	st.events = append(st.events, event(2, 2, jobs.StateFailed, *now))
	n.Step(ctx)
	*now = t0.Add(80 * time.Second)
	n.Step(ctx)
	if len(snd.sent) != 0 {
		t.Fatal("the second event restarts the window")
	}
	*now = t0.Add(111 * time.Second)
	n.Step(ctx)
	if len(snd.sent) != 1 || snd.sent[0].Title != "❌ 2× Backup failed · nas01" || len(st.events) != 0 {
		t.Fatalf("sent %+v, left %d", snd.sent, len(st.events))
	}
}

func TestNotifierMaxWait(t *testing.T) {
	n, st, snd, now := harness()
	ctx := context.Background()
	// An event every 30 s keeps the window open, but not beyond maxWait.
	for i := range 12 {
		*now = t0.Add(time.Duration(i) * 30 * time.Second)
		st.events = append(st.events, event(int64(i+1), int64(i+1), jobs.StateFailed, *now))
		n.Step(ctx)
	}
	if len(snd.sent) != 1 {
		t.Fatalf("after 5 min the batch must go out: %d messages", len(snd.sent))
	}
}

func TestNotifierDrops(t *testing.T) {
	ctx := context.Background()
	old := t0.Add(-2 * time.Hour)

	// Notifications off: everything is dropped, nothing sent.
	n, st, snd, _ := harness()
	st.settings.Topic = ""
	st.events = []store.NotifyEvent{event(1, 1, jobs.StateFailed, old)}
	n.Step(ctx)
	if len(snd.sent) != 0 || len(st.events) != 0 {
		t.Fatal("off: must drop")
	}

	// Default off (backup succeeded), cancelled, muted stack: dropped; the
	// failure of another stack is sent.
	n, st, snd, _ = harness()
	st.muted = map[int64]bool{2: true}
	st.events = []store.NotifyEvent{
		event(1, 1, jobs.StateSucceeded, old), event(2, 1, jobs.StateCancelled, old),
		event(3, 2, jobs.StateFailed, old), event(4, 3, jobs.StateFailed, old),
	}
	n.Step(ctx)
	if len(snd.sent) != 1 || snd.sent[0].Title != "❌ Backup failed · web" || len(st.events) != 0 {
		t.Fatalf("sent %+v, left %+v", snd.sent, st.events)
	}

	// Turned on by the admin.
	n, st, snd, _ = harness()
	st.settings.Events = map[string]bool{string(KeyBackupSucceeded): true}
	st.events = []store.NotifyEvent{event(1, 1, jobs.StateSucceeded, old)}
	n.Step(ctx)
	if len(snd.sent) != 1 {
		t.Fatal("enabled success must be sent")
	}
}

func TestNotifierRetryAndGiveUp(t *testing.T) {
	n, st, snd, now := harness()
	ctx := context.Background()
	snd.err = errors.New("connection refused")
	st.events = []store.NotifyEvent{event(1, 1, jobs.StateFailed, t0)}
	*now = t0.Add(2 * time.Minute)

	n.Step(ctx) // fails: backoff 1 min
	if len(st.events) != 1 || n.failures != 1 {
		t.Fatalf("failed send must keep the event: %+v", n)
	}
	snd.err = nil
	*now = now.Add(30 * time.Second)
	n.Step(ctx)
	if len(snd.sent) != 0 {
		t.Fatal("must wait for the backoff")
	}
	*now = now.Add(31 * time.Second)
	n.Step(ctx)
	if len(snd.sent) != 1 || len(st.events) != 0 || n.failures != 0 {
		t.Fatalf("retry: sent %d, left %d", len(snd.sent), len(st.events))
	}

	// Backoff grows and is capped.
	snd.err = errors.New("down")
	st.events = []store.NotifyEvent{event(2, 1, jobs.StateFailed, *now)}
	*now = now.Add(2 * time.Minute)
	for range 10 {
		n.nextAttempt = time.Time{}
		n.Step(ctx)
	}
	if got := n.nextAttempt.Sub(*now); got != maxBackoff {
		t.Errorf("backoff %v, want %v", got, maxBackoff)
	}

	for failures, want := range map[int]time.Duration{1: time.Minute, 2: 2 * time.Minute, 5: 16 * time.Minute,
		6: maxBackoff, 60: maxBackoff, 200: maxBackoff} {
		n.failures = failures
		if got := n.backoff(); got != want {
			t.Errorf("backoff after %d failures: %v, want %v", failures, got, want)
		}
	}

	// After a day the event is dropped.
	*now = now.Add(25 * time.Hour)
	n.Step(ctx)
	if len(st.events) != 0 {
		t.Fatal("must give up after 24 h")
	}
}
