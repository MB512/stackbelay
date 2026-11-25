package notify

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MB512/stackbelay/internal/docker"
	"github.com/MB512/stackbelay/internal/jobs"
	"github.com/MB512/stackbelay/internal/store"
)

func TestAdvance(t *testing.T) {
	// Each case feeds observations, one per minute, into a stack confirmed
	// in state start, and checks the events and the final state.
	tests := []struct {
		name      string
		start     string
		alerted   bool
		polls     []string
		wantKeys  []Key
		wantState string
	}{
		{"stays started", stateStarted, false, []string{"started", "started"}, nil, stateStarted},
		{"degraded after 3 polls", stateStarted, false, []string{"degraded", "degraded", "degraded"},
			[]Key{KeyStackDegraded}, stateDegraded},
		{"2 bad polls are not enough", stateStarted, false, []string{"degraded", "degraded", "started"}, nil, stateStarted},
		{"no reminders", stateStarted, false, []string{"degraded", "degraded", "degraded", "degraded", "degraded"},
			[]Key{KeyStackDegraded}, stateDegraded},
		{"degraded, then recovered after 2 polls", stateStarted, false,
			[]string{"degraded", "degraded", "degraded", "started", "started"},
			[]Key{KeyStackDegraded, KeyStackRecovered}, stateStarted},
		{"stopped outside StackBelay", stateStarted, false, []string{"stopped", "stopped", "stopped"},
			[]Key{KeyStackStopped}, stateStopped},
		{"stopped baseline, started by hand: quiet", stateStopped, false, []string{"started", "started"}, nil, stateStarted},
		{"stopped baseline, comes up degraded", stateStopped, false, []string{"degraded", "degraded", "degraded"},
			[]Key{KeyStackDegraded}, stateDegraded},
		{"degraded then stopped: one alert", stateStarted, false,
			[]string{"degraded", "degraded", "degraded", "stopped", "stopped", "stopped"}, []Key{KeyStackDegraded}, stateStopped},
		{"left down by a job (alerted), recovers", stateStopped, true, []string{"started", "started"},
			[]Key{KeyStackRecovered}, stateStarted},
		{"directory vanished", stateStarted, false, []string{"missing", "missing", "missing"},
			[]Key{KeyStackMissing}, stateMissing},
		{"unreadable", stateStarted, false, []string{"unknown", "unknown", "unknown"},
			[]Key{KeyStackUnreadable}, stateUnknown},
		{"unreadable while stopped: quiet", stateStopped, false, []string{"unknown", "unknown", "unknown"}, nil, stateUnknown},
		{"flapping between two bad states restarts the count", stateStarted, false,
			[]string{"degraded", "degraded", "unknown", "degraded", "degraded"}, nil, stateStarted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := t0
			h := store.StackHealth{StackID: 1, State: tt.start, Since: now, Alerted: tt.alerted}
			var got []Key
			for _, p := range tt.polls {
				now = now.Add(time.Minute)
				var ev *stackEvent
				h, ev = advance(h, observation{state: p}, now)
				if ev != nil {
					got = append(got, ev.key)
				}
			}
			if !keysEqual(got, tt.wantKeys) || h.State != tt.wantState {
				t.Errorf("events %v, state %q; want %v, %q", got, h.State, tt.wantKeys, tt.wantState)
			}
		})
	}
}

func keysEqual(a, b []Key) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestAdvanceRecoveryDuration(t *testing.T) {
	h := store.StackHealth{StackID: 1, State: stateStarted, Since: t0}
	var events []*stackEvent
	for i, p := range []string{"degraded", "degraded", "degraded", "degraded", "started", "started"} {
		now := t0.Add(time.Duration(i+1) * 3 * time.Minute)
		var ev *stackEvent
		if h, ev = advance(h, observation{state: p}, now); ev != nil {
			events = append(events, ev)
		}
	}
	// Bad from the first degraded poll (3 min) to the first good one (15 min).
	if len(events) != 2 || events[1].detail.Was != stateDegraded || events[1].detail.ForSeconds != 12*60 {
		t.Fatalf("events %+v", events[len(events)-1])
	}
}

func TestAfterJob(t *testing.T) {
	tests := []struct {
		job               store.Job
		baseline, alerted bool
	}{
		{store.Job{Type: jobs.TypeStop, State: jobs.StateSucceeded}, true, false},
		{store.Job{Type: jobs.TypeDown, State: jobs.StateSucceeded}, true, false},
		{store.Job{Type: jobs.TypeStop, State: jobs.StateFailed}, false, false},
		{store.Job{Type: jobs.TypeRestore, State: jobs.StateSucceeded, Options: store.JobOptions{NoStart: true}}, true, false},
		{store.Job{Type: jobs.TypeRestore, State: jobs.StateSucceeded}, false, false},
		{store.Job{Type: jobs.TypeBackup, State: jobs.StateFailed, Result: store.JobResult{ServicesDown: []string{"db"}}}, true, true},
		{store.Job{Type: jobs.TypeBackup, State: jobs.StateSucceeded}, false, false},
	}
	for _, tt := range tests {
		if b, a := afterJob(tt.job); b != tt.baseline || a != tt.alerted {
			t.Errorf("%s %s: got %v %v", tt.job.Type, tt.job.State, b, a)
		}
	}
}

// fakeMonitorStore keeps the monitor's data in memory.
type fakeMonitorStore struct {
	stacks  []store.Stack
	health  map[int64]store.StackHealth
	latest  map[int64]store.Job
	running []store.Job
	events  []store.NotifyEvent
}

func (f *fakeMonitorStore) ListStacks(context.Context) ([]store.Stack, error) { return f.stacks, nil }
func (f *fakeMonitorStore) StackHealths(context.Context) (map[int64]store.StackHealth, error) {
	out := map[int64]store.StackHealth{}
	for k, v := range f.health {
		out[k] = v
	}
	return out, nil
}
func (f *fakeMonitorStore) SaveStackHealth(_ context.Context, h store.StackHealth) error {
	f.health[h.StackID] = h
	return nil
}
func (f *fakeMonitorStore) LatestFinishedJobs(context.Context) (map[int64]store.Job, error) {
	return f.latest, nil
}
func (f *fakeMonitorStore) JobsInState(context.Context, jobs.State) ([]store.Job, error) {
	return f.running, nil
}
func (f *fakeMonitorStore) AddStackEvent(_ context.Context, kind string, stackID int64, detail string, at time.Time) error {
	f.events = append(f.events, store.NotifyEvent{Kind: kind, StackID: stackID, Detail: detail, CreatedAt: at})
	return nil
}

// fakeReader answers StackOverview per stack directory name.
type fakeReader struct {
	overviews map[string]docker.Overview
	err       error
}

func (f *fakeReader) StackOverview(_ context.Context, dir string) (docker.Overview, error) {
	if f.err != nil {
		return docker.Overview{}, f.err
	}
	return f.overviews[filepath.Base(dir)], nil
}

func up(services ...string) docker.Overview {
	ov := docker.Overview{Services: services}
	for _, s := range services {
		ov.Containers = append(ov.Containers, docker.Container{Name: s + "-1", Service: s, State: "running"})
	}
	return ov
}

func monitorHarness(t *testing.T) (*Monitor, *fakeMonitorStore, *fakeReader, *time.Time) {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{"web", "db"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name, "compose.yml"), []byte("services: {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	st := &fakeMonitorStore{health: map[int64]store.StackHealth{}, latest: map[int64]store.Job{},
		stacks: []store.Stack{{ID: 1, Name: "web", Present: true}, {ID: 2, Name: "db", Present: true}}}
	rd := &fakeReader{overviews: map[string]docker.Overview{"web": up("app", "redis"), "db": up("postgres")}}
	now := t0
	m := &Monitor{Store: st, Docker: rd, StacksRoot: root, Logger: slog.New(slog.DiscardHandler),
		Now: func() time.Time { return now }}
	return m, st, rd, &now
}

func TestMonitorStep(t *testing.T) {
	m, st, rd, now := monitorHarness(t)
	ctx := context.Background()
	poll := func(n int) {
		for range n {
			*now = now.Add(time.Minute)
			m.Step(ctx)
		}
	}

	poll(1) // baseline
	if len(st.health) != 2 || st.health[1].State != stateStarted || len(st.events) != 0 {
		t.Fatalf("baseline: %+v %+v", st.health, st.events)
	}

	// redis crashes: an alert after three polls, with the detail.
	ov := up("app", "redis")
	ov.Containers[1].State, ov.Containers[1].ExitCode = "exited", 1
	rd.overviews["web"] = ov
	poll(3)
	if len(st.events) != 1 || st.events[0].Kind != string(KeyStackDegraded) || st.events[0].StackID != 1 ||
		st.events[0].Detail != `{"running":1,"total":2,"problems":["redis is not running"]}` {
		t.Fatalf("degraded: %+v", st.events)
	}

	// A user stops db with a Stop job: quiet.
	st.running = []store.Job{{ID: "J1", StackID: 2, Type: jobs.TypeStop}}
	poll(1)
	st.running = nil
	st.latest[2] = store.Job{ID: "J1", StackID: 2, Type: jobs.TypeStop, State: jobs.StateSucceeded}
	rd.overviews["db"] = docker.Overview{Services: []string{"postgres"},
		Containers: []docker.Container{{Service: "postgres", State: "exited"}}}
	poll(4)
	if len(st.events) != 1 || st.health[2].State != stateStopped {
		t.Fatalf("stop job must be quiet: %+v %+v", st.events, st.health[2])
	}

	// Docker stops answering: one host alert, the stacks keep their state.
	rd.err = errors.New("Cannot connect to the Docker daemon")
	poll(4)
	if len(st.events) != 2 || st.events[1].Kind != string(KeyStackUnreadable) || st.events[1].StackID != 0 ||
		st.health[1].State != stateDegraded {
		t.Fatalf("host alert: %+v", st.events)
	}
	rd.err = nil
	rd.overviews["web"] = up("app", "redis")
	poll(1)
	if len(st.events) != 3 || st.events[2].Kind != string(KeyStackRecovered) || st.events[2].StackID != 0 {
		t.Fatalf("host recovery: %+v", st.events)
	}
	poll(1)
	if len(st.events) != 4 || st.events[3].Kind != string(KeyStackRecovered) || st.events[3].StackID != 1 {
		t.Fatalf("web recovery: %+v", st.events)
	}

	// The directory of db vanishes.
	if err := os.RemoveAll(filepath.Join(m.StacksRoot, "db")); err != nil {
		t.Fatal(err)
	}
	poll(3)
	if len(st.events) != 5 || st.events[4].Kind != string(KeyStackMissing) || st.events[4].StackID != 2 {
		t.Fatalf("missing: %+v", st.events)
	}
}

func TestMonitorFailedJobLeftDown(t *testing.T) {
	m, st, rd, now := monitorHarness(t)
	ctx := context.Background()
	m.Step(ctx) // baseline: both started

	// A backup fails and leaves web down; its job message reported it.
	st.latest[1] = store.Job{ID: "J2", StackID: 1, Type: jobs.TypeBackup, State: jobs.StateFailed,
		Result: store.JobResult{ServicesDown: []string{"app", "redis"}}}
	rd.overviews["web"] = docker.Overview{Services: []string{"app", "redis"},
		Containers: []docker.Container{{Service: "app", State: "exited"}, {Service: "redis", State: "exited"}}}
	for range 4 {
		*now = now.Add(time.Minute)
		m.Step(ctx)
	}
	if len(st.events) != 0 || !st.health[1].Alerted {
		t.Fatalf("no second alert: %+v %+v", st.events, st.health[1])
	}
	// Started by hand: the monitor reports the recovery.
	rd.overviews["web"] = up("app", "redis")
	for range 2 {
		*now = now.Add(time.Minute)
		m.Step(ctx)
	}
	if len(st.events) != 1 || st.events[0].Kind != string(KeyStackRecovered) {
		t.Fatalf("recovery: %+v", st.events)
	}
}

func TestFormatStackEvents(t *testing.T) {
	tests := []struct {
		ev        Event
		wantTitle string
		wantBody  string
	}{
		{Event{Key: KeyStackDegraded, Stack: "nextcloud", Detail: StackDetail{Running: 3, Total: 4, Problems: []string{"nextcloud-redis-1 is restarting"}}},
			"⚠️ nextcloud · stack degraded", "nas01\n3 of 4 services running\nnextcloud-redis-1 is restarting"},
		{Event{Key: KeyStackStopped, Stack: "paperless"},
			"⚠️ paperless · stopped outside StackBelay", "nas01\nNo service runs, and no StackBelay job stopped it."},
		{Event{Key: KeyStackRecovered, Stack: "nextcloud", Detail: StackDetail{Was: "degraded", ForSeconds: 720}},
			"✅ nextcloud · stack recovered", "nas01\nAll services running · degraded for 12 min"},
		{Event{Key: KeyStackUnreadable}, "❔ cannot read stack states · nas01", "nas01\nDocker did not answer for any stack."},
		{Event{Key: KeyStackMissing, Stack: "db"}, "⚠️ db · directory missing", "nas01\nThe stack directory is gone."},
	}
	for _, tt := range tests {
		m := Format("nas01", []Event{tt.ev}, utc)
		if m.Title != tt.wantTitle || m.Body != tt.wantBody {
			t.Errorf("%s: %q\n%s", tt.ev.Key, m.Title, m.Body)
		}
	}
	// Combined with a job event.
	m := Format("nas01", []Event{
		{Key: KeyStackDegraded, Stack: "nextcloud", Detail: StackDetail{Problems: []string{"redis is not running"}}},
		{Key: KeyBackupFailed, Job: finished("web", jobs.TypeBackup, jobs.StateFailed, "final snapshot: x", store.JobResult{})},
	}, utc)
	if m.Body != "⚠️ nextcloud: stack degraded (redis is not running)\n❌ web: backup failed (snapshot failed)" {
		t.Errorf("combined: %s", m.Body)
	}
}
