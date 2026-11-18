package web

import (
	"net/http"

	"github.com/MB512/stackvault/internal/store"
)

// adminRecentJobs is the number of jobs of all stacks on the admin Jobs tab.
const adminRecentJobs = 50

// adminJobsLive is the polled part of the admin Jobs tab.
type adminJobsLive struct {
	Active []store.Job // running, then queued, each oldest first
	Recent []store.Job // newest first, all stacks
}

// adminJobs shows the job queue and the recent jobs of all stacks (admin
// only).
func (s *Server) adminJobs(w http.ResponseWriter, r *http.Request) {
	live, err := s.loadAdminJobs(r)
	if err != nil {
		s.internalError(w, r, "listing jobs", err)
		return
	}
	s.render(w, r, http.StatusOK, "admin_jobs.html", pageData{Title: "Jobs", Data: live})
}

// adminJobsLiveFragment answers the HTMX poll of the admin Jobs tab.
func (s *Server) adminJobsLiveFragment(w http.ResponseWriter, r *http.Request) {
	live, err := s.loadAdminJobs(r)
	if err != nil {
		s.internalError(w, r, "listing jobs", err)
		return
	}
	s.renderPartial(w, "admin_jobs.html", "admin-jobs-live", live)
}

func (s *Server) loadAdminJobs(r *http.Request) (adminJobsLive, error) {
	var live adminJobsLive
	var err error
	if live.Active, err = s.Store.ActiveJobs(r.Context()); err != nil {
		return live, err
	}
	live.Recent, err = s.Store.ListJobs(r.Context(), 0, adminRecentJobs)
	return live, err
}
