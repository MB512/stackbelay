package notify

import (
	"regexp"
	"strings"
)

// The raw error of a job can contain repository locations, paths and
// restic or docker output, so messages carry a short classified reason
// instead. The patterns follow the error texts of internal/jobs,
// internal/docker, the worker and the dispatcher; the first match wins, so
// the more specific ones come first.
var reasons = []struct {
	contains []string // all must occur
	reason   string
}{
	{[]string{"not rolled back (no repository)"}, "failed without rollback (no repository)"},
	{[]string{"downtime timeout"}, "downtime timeout"},
	{[]string{"job timeout"}, "job timeout"},
	{[]string{"locked by another process"}, "repository locked (maintenance running?)"},
	{[]string{"repository not reachable"}, "repository unreachable"},
	{[]string{"stack has no repository"}, "no repository"},
	{[]string{"stack is not healthy"}, "services did not become healthy"},
	{[]string{"pulling images"}, "pull failed"},
	{[]string{"was changed after it was opened in the editor"}, "compose file changed meanwhile"},
	{[]string{"last restore of stack", "did not complete"}, "previous restore incomplete"},
	{[]string{"no service of the stack is running"}, "stack not running"},
	{[]string{"has no containers to start"}, "stack has no containers"},
	{[]string{"restarting stack"}, "restart failed"},
	{[]string{"starting restored stack"}, "start failed"},
	{[]string{"starting stack"}, "start failed"},
	{[]string{"stopping stack"}, "stop failed"},
	{[]string{"final snapshot"}, "snapshot failed"},
	{[]string{"safety snapshot"}, "safety snapshot failed"},
	{[]string{"restoring snapshot"}, "restore failed"},
	{[]string{"snapshot", "not found"}, "snapshot not found"},
	{[]string{"worker container"}, "worker lost"},
}

// failedServices matches the list docker.CLI's health wait reports, e.g.
// "services failed: gitea (unhealthy), redis (exited)".
var failedServices = regexp.MustCompile(`services failed: ([^;\n]+)`)

// serviceEntry is one entry of that list. Compose service names consist
// of these characters; anything else is not echoed.
var serviceEntry = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]* \((unhealthy|exited|dead)\)$`)

// Reason returns the short reason for a job error ("repository
// unreachable", "service gitea (unhealthy)"), or "see job log" if the
// error is not known.
func Reason(errMsg string) string {
	lower := strings.ToLower(errMsg)
	// A failed rollback matters more than what made the change fail.
	if strings.Contains(lower, "rollback to snapshot") && strings.Contains(lower, "failed") {
		return "rollback failed"
	}
	if m := failedServices.FindStringSubmatch(errMsg); m != nil {
		var ok []string
		for entry := range strings.SplitSeq(m[1], ", ") {
			if entry = strings.TrimSpace(entry); serviceEntry.MatchString(entry) {
				ok = append(ok, entry)
			}
		}
		if len(ok) > 0 {
			if len(ok) == 1 {
				return "service " + ok[0]
			}
			return "services " + strings.Join(ok, ", ")
		}
	}
outer:
	for _, r := range reasons {
		for _, c := range r.contains {
			if !strings.Contains(lower, c) {
				continue outer
			}
		}
		return r.reason
	}
	return "see job log"
}
