package notify

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/MB512/stackbelay/internal/jobs"
	"github.com/MB512/stackbelay/internal/store"
)

var (
	t0  = time.Date(2026, 11, 25, 3, 0, 0, 0, time.UTC)
	utc = time.UTC
)

func finished(stack string, typ jobs.Type, state jobs.State, errMsg string, res store.JobResult) store.Job {
	return store.Job{StackName: stack, Type: typ, State: state, Error: errMsg, Result: res,
		Trigger: store.TriggerSchedule, StartedAt: t0, FinishedAt: t0.Add(14 * time.Minute)}
}

func TestShortHost(t *testing.T) {
	for in, want := range map[string]string{"nas01": "nas01", "nas01.home.example.com": "nas01", "": ""} {
		if got := ShortHost(in); got != want {
			t.Errorf("ShortHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFormatOne(t *testing.T) {
	tests := []struct {
		name      string
		ev        Event
		wantTitle string
		wantBody  []string // lines
		wantPrio  int
	}{
		{"backup failed, stack running again",
			Event{KeyBackupFailed, finished("immich", jobs.TypeBackup, jobs.StateFailed, "repository not reachable: x", store.JobResult{})},
			"❌ Backup failed · immich",
			[]string{"nas01 · scheduled · 25 Nov 03:00–03:14", "Services that ran before are running.", "Reason: repository unreachable"},
			PriorityHigh},
		{"update left the stack down",
			Event{KeyLeftDown, finished("gitea", jobs.TypeUpdate, jobs.StateFailed,
				"update failed: x\nrollback to snapshot 1a2b3c4d failed: y", store.JobResult{ServicesDown: []string{"gitea", "db"}})},
			"🚨 Update failed · gitea",
			[]string{"nas01 · scheduled · 25 Nov 03:00–03:14", "Stack is DOWN. Not running: gitea, db", "Reason: rollback failed"},
			PriorityUrgent},
		{"update rolled back",
			Event{KeyRollback, finished("gitea", jobs.TypeUpdate, jobs.StateFailed,
				"update failed and was rolled back to snapshot 1a2b3c4d: stack is not healthy: services failed: gitea (unhealthy)",
				store.JobResult{RolledBack: true})},
			"↩️ Update rolled back · gitea",
			[]string{"nas01 · scheduled · 25 Nov 03:00–03:14", "Stack runs the previous version.", "Reason: service gitea (unhealthy)"},
			PriorityHigh},
		{"update applied",
			Event{KeyUpdateSucceeded, finished("immich", jobs.TypeUpdate, jobs.StateSucceeded, "",
				store.JobResult{ImagesChanged: []string{"server", "machine-learning"}})},
			"✅ Update applied · immich",
			[]string{"nas01 · scheduled · 25 Nov 03:00–03:14", "Changed: server, machine-learning"},
			PriorityLow},
		{"stop failed: no state line",
			Event{KeyOperationFailed, finished("web", jobs.TypeStop, jobs.StateFailed, "stopping stack: exit status 1", store.JobResult{})},
			"❌ Stop failed · web",
			[]string{"nas01 · scheduled · 25 Nov 03:00–03:14", "Reason: stop failed"},
			PriorityHigh},
		{"worker lost: state unknown",
			Event{KeyBackupFailed, finished("web", jobs.TypeBackup, jobs.StateFailed, "the worker container disappeared without reporting a result", store.JobResult{})},
			"❌ Backup failed · web",
			[]string{"nas01 · scheduled · 25 Nov 03:00–03:14", "Reason: worker lost"},
			PriorityHigh},
		{"host cleanup",
			Event{KeyCleanupSucceeded, finished("", jobs.TypeCleanup, jobs.StateSucceeded, "",
				store.JobResult{RemovedImages: 3, RemovedContainers: 1, FreedBytes: 3 << 29})},
			"✅ Host cleanup succeeded · host",
			[]string{"nas01 · scheduled · 25 Nov 03:00–03:14", "Removed 3 images and 1 containers, freed 1.5 GiB."},
			PriorityLow},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := Format("nas01", []Event{tt.ev}, utc)
			if m.Title != tt.wantTitle || m.Body != strings.Join(tt.wantBody, "\n") || m.Priority != tt.wantPrio {
				t.Errorf("got %q\n%s\n(priority %d)", m.Title, m.Body, m.Priority)
			}
		})
	}
}

func TestFormatManualAndOvernight(t *testing.T) {
	j := finished("web", jobs.TypeBackup, jobs.StateSucceeded, "", store.JobResult{})
	j.Trigger, j.StartedAt, j.FinishedAt = store.TriggerManual, t0.Add(-4*time.Hour), t0.Add(time.Minute)
	m := Format("nas01", []Event{{KeyBackupSucceeded, j}}, utc)
	if m.Body != "nas01 · manual · 24 Nov 23:00–25 Nov 03:01" {
		t.Errorf("body %q", m.Body)
	}
}

func TestFormatMany(t *testing.T) {
	backups := []Event{
		{KeyBackupFailed, finished("immich", jobs.TypeBackup, jobs.StateFailed, "repository not reachable: x", store.JobResult{})},
		{KeyBackupFailed, finished("gitea", jobs.TypeBackup, jobs.StateFailed, "repository not reachable: x", store.JobResult{})},
	}
	m := Format("nas01", backups, utc)
	if m.Title != "❌ 2× Backup failed · nas01" || m.Priority != PriorityHigh ||
		m.Body != "❌ immich: backup failed (repository unreachable)\n❌ gitea: backup failed (repository unreachable)" {
		t.Errorf("same type: %q\n%s", m.Title, m.Body)
	}

	mixed := append(backups, Event{KeyLeftDown, finished("db", jobs.TypeBackup, jobs.StateFailed, "restarting stack: x",
		store.JobResult{ServicesDown: []string{"postgres"}})})
	m = Format("nas01", mixed, utc)
	if m.Title != "🚨 3 events · nas01" || m.Priority != PriorityUrgent ||
		!strings.HasSuffix(m.Body, "🚨 db: backup failed (DOWN: postgres)") {
		t.Errorf("mixed: %q\n%s", m.Title, m.Body)
	}
}

func TestFormatLimit(t *testing.T) {
	var many []Event
	for i := range 200 {
		many = append(many, Event{KeyBackupSucceeded,
			finished(fmt.Sprintf("stack-with-a-long-name-%03d", i), jobs.TypeBackup, jobs.StateSucceeded, "", store.JobResult{})})
	}
	m := Format("nas01", many, utc)
	if len(m.Body) > maxBodyBytes || !strings.HasSuffix(m.Body, " more") || !strings.Contains(m.Body, "stack-with-a-long-name-000") {
		t.Errorf("body of %d bytes, ends %q", len(m.Body), m.Body[len(m.Body)-40:])
	}
	if m.Title != "✅ 200× Backup succeeded · nas01" {
		t.Errorf("title %q", m.Title)
	}
}
