package web

import (
	"context"
	"fmt"
	"net/http"
	"slices"

	"github.com/MB512/stackvault/internal/auth"
	"github.com/MB512/stackvault/internal/store"
)

// Restore from another host (issue #1): a stack lists and restores the
// snapshots of its source host. Backups always use this host's name.

// snapshotHost is the host whose snapshots st lists and restores: its
// source host, else this host ("" = all hosts if this host is unknown).
func (s *Server) snapshotHost(st store.Stack) string {
	if st.SourceHost != "" {
		return st.SourceHost
	}
	return s.Hostname
}

// foreignSource reports whether st lists another host's snapshots.
func (s *Server) foreignSource(st store.Stack) bool {
	return st.SourceHost != "" && st.SourceHost != s.Hostname
}

// mayRestore reports whether user may restore snapshots of st: the restore
// right, and for another host's snapshots admin.
func (s *Server) mayRestore(user store.User, perm store.Permission, st store.Stack) bool {
	if s.foreignSource(st) && !user.IsAdmin {
		return false
	}
	return auth.Allowed(user, perm, auth.ActionRestore)
}

// sourceHostOptions are the hosts an admin can choose as source host of
// st: this host and every host with cached snapshots of the stack.
func (s *Server) sourceHostOptions(ctx context.Context, st store.Stack) ([]string, error) {
	var hosts []string
	if s.Hostname != "" {
		hosts = append(hosts, s.Hostname)
	}
	repo, err := s.Store.RepositoryInfoForStack(ctx, st.ID)
	if err == nil {
		found, err := s.Store.StackSnapshotHosts(ctx, repo.ID, st.Name)
		if err != nil {
			return nil, err
		}
		for _, h := range found {
			if !slices.Contains(hosts, h) {
				hosts = append(hosts, h)
			}
		}
	}
	if st.SourceHost != "" && !slices.Contains(hosts, st.SourceHost) {
		hosts = append(hosts, st.SourceHost)
	}
	return hosts, nil
}

// setSourceHost sets the host whose snapshots a stack lists and restores
// (admin only). Only hosts from sourceHostOptions are accepted.
func (s *Server) setSourceHost(w http.ResponseWriter, r *http.Request) {
	st, _, ok := s.loadStack(w, r, auth.ActionView)
	if !ok {
		return
	}
	ctx := r.Context()
	options, err := s.sourceHostOptions(ctx, st)
	if err != nil {
		s.internalError(w, r, "listing snapshot hosts", err)
		return
	}
	host := r.PostFormValue("source_host")
	if !slices.Contains(options, host) {
		s.renderError(w, r, http.StatusBadRequest, "Unknown host.")
		return
	}
	stored := host
	if host == s.Hostname {
		stored = "" // this host
	}
	if err := s.Store.SetStackSourceHost(ctx, st.ID, stored); err != nil {
		s.internalError(w, r, "saving source host", err)
		return
	}
	s.audit(r, store.AuditEntry{UserID: authFrom(ctx).User.ID, Action: "stack.source_host", StackID: st.ID,
		Detail: fmt.Sprintf("source host %q", host)})
	http.Redirect(w, r, settingsURL(st), http.StatusSeeOther) //nolint:gosec // G710: same-site path, see settingsURL
}
