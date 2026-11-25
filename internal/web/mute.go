package web

import (
	"net/http"
	"strings"
	"time"

	"github.com/MB512/stackbelay/internal/store"
)

// The temporary notification mute of admins (issue #50). The state is in
// notify.Mutes, shared with the stack monitor.

// muteBadge is the mute control in the page header.
type muteBadge struct {
	Available bool   // an admin, and the mute is configured
	Active    bool   // muted now
	Seconds   int    // left, for the countdown (forms.js)
	Until     string // end time, without JavaScript
	Back      string // the page to return to after the toggle
}

func (s *Server) muteBadge(r *http.Request, u store.User) muteBadge {
	if !u.IsAdmin || s.Mutes == nil {
		return muteBadge{}
	}
	b := muteBadge{Available: true, Back: r.URL.Path}
	now := s.now()
	if until := s.Mutes.Until(u.ID, now); !until.IsZero() {
		b.Active, b.Seconds = true, int(until.Sub(now).Round(time.Second).Seconds())
		b.Until = until.In(s.loc()).Format("15:04")
	}
	return b
}

// markMuted reports whether a job u starts now is muted, and records the
// stack it touches for the monitor (stackID 0: a host job).
func (s *Server) markMuted(u store.User, stackID int64) bool {
	if !u.IsAdmin || s.Mutes == nil || s.Mutes.Until(u.ID, s.now()).IsZero() {
		return false
	}
	s.Mutes.Touch(u.ID, stackID)
	return true
}

// setAdminMute starts, resets or ends the admin's mute (admin only) and
// returns to the page it was used on.
func (s *Server) setAdminMute(w http.ResponseWriter, r *http.Request) {
	u := authFrom(r.Context()).User
	if s.Mutes == nil {
		s.renderError(w, r, http.StatusNotFound, "Page not found.")
		return
	}
	on := r.PostFormValue("on") == "true"
	if on {
		s.Mutes.Mute(u.ID, s.now())
		s.audit(r, store.AuditEntry{UserID: u.ID, Action: "notify.mute"})
	} else {
		s.Mutes.Unmute(u.ID)
		s.audit(r, store.AuditEntry{UserID: u.ID, Action: "notify.unmute"})
	}
	http.Redirect(w, r, localPath(r.PostFormValue("back")), http.StatusSeeOther) //nolint:gosec // G710: a local path, see localPath
}

// localPath returns p if it is a path on this site, else "/". It keeps a
// redirect from leaving the site ("//host", "/\\host", "https://...").
func localPath(p string) string {
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.ContainsAny(p, "\\\r\n") {
		return "/"
	}
	return p
}
