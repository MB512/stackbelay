package notify

import "testing"

func TestReason(t *testing.T) {
	// Error texts as the pipelines, the worker and the dispatcher write them.
	tests := []struct{ err, want string }{
		{"repository not reachable: Fatal: unable to open config file: Stat: Get \"https://user@nas:8000/repo/config\"", "repository unreachable"},
		{"update failed and was rolled back to snapshot 1a2b3c4d: stack is not healthy: services failed: gitea (unhealthy)", "service gitea (unhealthy)"},
		{"compose change failed and was rolled back to snapshot 1a2b3c4d: stack is not healthy: services failed: app (exited), db (unhealthy)", "services app (exited), db (unhealthy)"},
		{"update failed: stack is not healthy: services failed: gitea (unhealthy)\nrollback to snapshot 1a2b3c4d failed: restoring snapshot: exit status 1", "rollback failed"},
		{"update failed and was not rolled back (no repository): starting stack: exit status 1", "failed without rollback (no repository)"},
		{"downtime timeout (1h0m0s, STACKBELAY_DOWNTIME_TIMEOUT) exceeded: the final snapshot was aborted", "downtime timeout"},
		{"job timeout (6h0m0s) exceeded: restoring snapshot: signal: killed", "job timeout"},
		{"pulling images (the stack was not changed): exit status 18", "pull failed"},
		{"final snapshot: exit status 3\nrestarting stack: exit status 1", "restart failed"},
		{"final snapshot: exit status 3", "snapshot failed"},
		{"stopping stack: exit status 1", "stop failed"},
		{"stack is not healthy: context deadline exceeded", "services did not become healthy"},
		{"starting job: the stack has no repository", "no repository"},
		{"the worker container disappeared without reporting a result", "worker lost"},
		{"the worker container stopped (exit code 137) without reporting a result", "worker lost"},
		{"snapshot 1a2b3c4d not found (or ambiguous)", "snapshot not found"},
		{"compose.yaml was changed after it was opened in the editor; nothing was applied", "compose file changed meanwhile"},
		{"something nobody expected", "see job log"},
		// A service list with something that is not a service name is not
		// echoed.
		{"stack is not healthy: services failed: $(evil) (unhealthy)", "services did not become healthy"},
	}
	for _, tt := range tests {
		if got := Reason(tt.err); got != tt.want {
			t.Errorf("Reason(%q) = %q, want %q", tt.err, got, tt.want)
		}
	}
}
