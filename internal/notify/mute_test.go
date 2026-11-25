package notify

import (
	"context"
	"testing"
	"time"

	"github.com/MB512/stackbelay/internal/jobs"
	"github.com/MB512/stackbelay/internal/store"
)

func TestMutes(t *testing.T) {
	var m Mutes // the zero value is ready to use
	now := t0
	if !m.Until(1, now).IsZero() || m.StackMuted(7, now) {
		t.Fatal("nobody is muted at first")
	}
	m.Extend(1, now) // not muted: nothing to extend
	if !m.Until(1, now).IsZero() {
		t.Fatal("Extend must not start a mute")
	}

	m.Mute(1, now)
	m.Touch(1, 7)
	m.Touch(1, 0) // a host job touches no stack
	if got := m.Until(1, now); !got.Equal(now.Add(MuteDuration)) || !m.StackMuted(7, now) || m.StackMuted(8, now) {
		t.Fatalf("muted until %v", got)
	}
	if !m.Until(2, now).IsZero() {
		t.Fatal("only the admin who muted is muted")
	}

	// Page requests keep the mute alive.
	now = now.Add(4 * time.Minute)
	m.Extend(1, now)
	now = now.Add(4 * time.Minute)
	if m.Until(1, now).IsZero() || !m.StackMuted(7, now) {
		t.Fatal("an extended mute must still be active")
	}
	// No request for 5 minutes: the mute ends, and with it the stack's.
	now = now.Add(MuteDuration)
	if !m.Until(1, now).IsZero() || m.StackMuted(7, now) {
		t.Fatal("the mute must have ended")
	}
	m.Extend(1, now)
	if !m.Until(1, now).IsZero() {
		t.Fatal("an ended mute must not come back")
	}

	m.Mute(1, now)
	m.Touch(1, 7)
	m.Unmute(1)
	if !m.Until(1, now).IsZero() || m.StackMuted(7, now) {
		t.Fatal("unmute ends everything")
	}
	var nilMutes *Mutes
	if nilMutes.StackMuted(7, now) {
		t.Fatal("nil Mutes mutes nothing")
	}
}

func TestNotifierDropsMutedJobs(t *testing.T) {
	n, st, snd, _ := harness()
	ev := event(1, 1, jobs.StateFailed, t0.Add(-time.Hour))
	ev.Job.Options.NotifyMuted = true
	st.events = []store.NotifyEvent{ev}
	n.Step(context.Background())
	if len(snd.sent) != 0 || len(st.events) != 0 {
		t.Fatalf("a muted job must be dropped: sent %+v", snd.sent)
	}
}

func TestMonitorSkipsMutedStack(t *testing.T) {
	m, st, rd, now := monitorHarness(t)
	ctx := context.Background()
	m.Mutes = &Mutes{}
	m.Step(ctx) // baseline

	// The admin mutes, updates web (muted job), and it ends up degraded.
	m.Mutes.Mute(42, *now)
	m.Mutes.Touch(42, 1)
	ov := up("app", "redis")
	ov.Containers[1].State = "restarting"
	rd.overviews["web"] = ov
	for range 4 {
		*now = now.Add(time.Minute)
		m.Step(ctx)
	}
	if len(st.events) != 0 {
		t.Fatalf("quiet while muted: %+v", st.events)
	}
	// The admin stops working: once the mute ends, the usual three polls.
	*now = now.Add(MuteDuration)
	for range 3 {
		*now = now.Add(time.Minute)
		m.Step(ctx)
	}
	if len(st.events) != 1 || st.events[0].Kind != string(KeyStackDegraded) {
		t.Fatalf("alert after the mute: %+v", st.events)
	}
}
