package notify

import (
	"strings"
	"testing"

	"github.com/MB512/stackbelay/internal/jobs"
	"github.com/MB512/stackbelay/internal/store"
)

func TestClassify(t *testing.T) {
	job := func(typ jobs.Type, state jobs.State, res store.JobResult) store.Job {
		return store.Job{Type: typ, State: state, Result: res}
	}
	none := Key("")
	tests := []struct {
		name string
		job  store.Job
		want Key
	}{
		{"backup failed", job(jobs.TypeBackup, jobs.StateFailed, store.JobResult{}), KeyBackupFailed},
		{"backup succeeded", job(jobs.TypeBackup, jobs.StateSucceeded, store.JobResult{}), KeyBackupSucceeded},
		{"backup cancelled", job(jobs.TypeBackup, jobs.StateCancelled, store.JobResult{}), none},
		{"backup running", job(jobs.TypeBackup, jobs.StateRunning, store.JobResult{}), none},
		{"backup left the stack down", job(jobs.TypeBackup, jobs.StateFailed, store.JobResult{ServicesDown: []string{"db"}}), KeyLeftDown},
		{"update applied", job(jobs.TypeUpdate, jobs.StateSucceeded, store.JobResult{ImagesChanged: []string{"app"}}), KeyUpdateSucceeded},
		{"update unchanged", job(jobs.TypeUpdate, jobs.StateSucceeded, store.JobResult{Unchanged: true}), none},
		{"update failed", job(jobs.TypeUpdate, jobs.StateFailed, store.JobResult{}), KeyUpdateFailed},
		{"update rolled back", job(jobs.TypeUpdate, jobs.StateFailed, store.JobResult{RolledBack: true}), KeyRollback},
		{"rollback failed, stack down", job(jobs.TypeUpdate, jobs.StateFailed, store.JobResult{ServicesDown: []string{"app"}}), KeyLeftDown},
		{"compose change rolled back", job(jobs.TypeComposeApply, jobs.StateFailed, store.JobResult{RolledBack: true}), KeyRollback},
		{"compose change applied", job(jobs.TypeComposeApply, jobs.StateSucceeded, store.JobResult{}), KeyComposeSucceeded},
		{"restore failed", job(jobs.TypeRestore, jobs.StateFailed, store.JobResult{}), KeyRestoreFailed},
		{"restore succeeded", job(jobs.TypeRestore, jobs.StateSucceeded, store.JobResult{}), KeyRestoreSucceeded},
		{"stop failed", job(jobs.TypeStop, jobs.StateFailed, store.JobResult{}), KeyOperationFailed},
		{"down succeeded", job(jobs.TypeDown, jobs.StateSucceeded, store.JobResult{}), KeyOperationSucceeded},
		{"cleanup failed", job(jobs.TypeCleanup, jobs.StateFailed, store.JobResult{}), KeyCleanupFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := Classify(tt.job)
			if got != tt.want || ok != (tt.want != none) {
				t.Errorf("got %q, %v; want %q", got, ok, tt.want)
			}
		})
	}
}

func TestEnabledDefaults(t *testing.T) {
	// Defaults (issue #47): failures, rollbacks and left-down are on,
	// successes off.
	for _, e := range Events {
		// Monitor events (issue #49) are all on by default.
		want := e.Key == KeyRollback || e.Key == KeyLeftDown || e.Key == KeySummary ||
			strings.HasSuffix(e.Label, "failed") || strings.HasPrefix(string(e.Key), "stack.")
		if got := Enabled(nil, e.Key); got != want {
			t.Errorf("%s: default %v, want %v", e.Key, got, want)
		}
	}
	settings := map[string]bool{string(KeyBackupFailed): false, string(KeyBackupSucceeded): true}
	if Enabled(settings, KeyBackupFailed) || !Enabled(settings, KeyBackupSucceeded) || !Enabled(settings, KeyRollback) {
		t.Error("the admin's choice must override the default")
	}
	if p, _ := Info(KeyLeftDown); p.Priority != PriorityUrgent {
		t.Errorf("a stack left down is urgent: %+v", p)
	}
	for _, e := range Events {
		if e.Key != KeyLeftDown && e.Priority == PriorityUrgent {
			t.Errorf("%s must not be urgent", e.Key)
		}
	}
}
