package web

import (
	"net/http"
	"strconv"

	"github.com/MB512/stackbelay/internal/store"
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

// auditPageSize is the number of audit entries per page.
const auditPageSize = 100

// auditPage is the admin Audit log tab.
type auditPage struct {
	Entries []store.AuditEntry // newest first
	Before  int64              // this page starts below this id (0 = newest)
	Older   int64              // before of the next page, 0 if there is none
}

// adminAudit shows the audit log, newest first, a page at a time (admin
// only). ?before=<id> shows the entries older than that id.
func (s *Server) adminAudit(w http.ResponseWriter, r *http.Request) {
	var page auditPage
	if v := r.URL.Query().Get("before"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil || id <= 0 {
			s.renderError(w, r, http.StatusBadRequest, "Invalid page.")
			return
		}
		page.Before = id
	}
	// One more than shown tells whether there is an older page.
	entries, err := s.Store.ListAuditBefore(r.Context(), page.Before, auditPageSize+1)
	if err != nil {
		s.internalError(w, r, "listing audit log", err)
		return
	}
	if len(entries) > auditPageSize {
		entries = entries[:auditPageSize]
		page.Older = entries[len(entries)-1].ID
	}
	page.Entries = entries
	s.render(w, r, http.StatusOK, "admin_audit.html", pageData{Title: "Audit log", Data: page})
}
