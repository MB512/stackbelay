package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/MB512/stackvault/internal/auth"
	"github.com/MB512/stackvault/internal/jobs"
	"github.com/MB512/stackvault/internal/stacks"
	"github.com/MB512/stackvault/internal/store"
)

// stackRow is one line of the stack list.
type stackRow struct {
	store.StackSummary
	Rights string // what the user may do besides viewing, e.g. "backup, logs"
}

// rightsText lists the actions besides viewing that user may perform with perm.
func rightsText(user store.User, perm store.Permission) string {
	var names []string
	for _, act := range []auth.Action{auth.ActionBackup, auth.ActionRestore, auth.ActionUpdate, auth.ActionLogs} {
		if auth.Allowed(user, perm, act) {
			names = append(names, act.String())
		}
	}
	return strings.Join(names, ", ")
}

// stackList shows the stacks the user may see.
func (s *Server) stackList(w http.ResponseWriter, r *http.Request) {
	a := authFrom(r.Context())
	sums, err := s.Store.StackSummaries(r.Context(), s.Hostname)
	if err != nil {
		s.internalError(w, r, "listing stacks", err)
		return
	}
	var perms map[int64]store.Permission
	if !a.User.IsAdmin {
		if perms, err = s.Store.PermissionsForUser(r.Context(), a.User.ID); err != nil {
			s.internalError(w, r, "listing stacks", err)
			return
		}
	}
	var rows []stackRow
	for _, sum := range sums {
		p := perms[sum.ID] // zero Permission (no grant) if missing, also for admins
		if !auth.Allowed(a.User, p, auth.ActionView) {
			continue
		}
		rows = append(rows, stackRow{StackSummary: sum, Rights: rightsText(a.User, p)})
	}
	s.render(w, r, http.StatusOK, "stacks.html", pageData{Title: "Stacks", Data: rows})
}

// stackTab is one tab in the header of the stack pages.
type stackTab struct {
	Label  string
	URL    string
	Active bool
}

// stackHeader is the common top of all stack pages: name, status and the
// tabs the user may open.
type stackHeader struct {
	Stack store.Stack
	Tabs  []stackTab
}

// Tab ids, also the last path segment of the tab ("" = the stack's root).
const (
	tabStatus   = ""
	tabBackups  = "backups"
	tabLogs     = "logs"
	tabCompose  = "compose"
	tabSettings = "settings"
)

// newStackHeader builds the tab list for user, whose permission on the
// stack is perm. Every tab has its own permission check here and again in
// its handler; tabs the user may not open are not shown.
func (s *Server) newStackHeader(user store.User, perm store.Permission, st store.Stack, active string) stackHeader {
	base := "/stacks/" + url.PathEscape(st.Name)
	tabs := []stackTab{
		{Label: "Status", URL: base, Active: active == tabStatus},
		{Label: "Backups", URL: base + "/" + tabBackups, Active: active == tabBackups},
	}
	if auth.Allowed(user, perm, auth.ActionLogs) {
		tabs = append(tabs, stackTab{Label: "Logs", URL: base + "/" + tabLogs, Active: active == tabLogs})
	}
	if user.IsAdmin {
		// Editing the compose file is admin-only: it is equivalent to root
		// on the host.
		tabs = append(tabs, stackTab{Label: "Compose", URL: base + "/" + tabCompose, Active: active == tabCompose})
		tabs = append(tabs, stackTab{Label: "Settings", URL: base + "/" + tabSettings, Active: active == tabSettings})
	}
	return stackHeader{Stack: st, Tabs: tabs}
}

// backupsTab is the data of the "Backups" tab: repository and snapshots.
// It looks the same for every user who may view the stack.
type backupsTab struct {
	stackHeader // embedded: .Stack and .Tabs are available directly
	CanRestore  bool
	Repo        *store.RepositoryInfo // nil = no repository configured
	Snapshots   []store.CachedSnapshot
	// CSRF and the refresh result, for the "snapshots" partial, which is
	// also rendered on its own as the answer to an HTMX refresh.
	CSRF                      string
	RefreshInfo, RefreshError string
	Host                      string // snapshots are shown for this host ("" = all)
	OtherSnapshots            int    // snapshots in the repository of other stacks or hosts
	// NextScheduled is the next scheduled backup; zero if none.
	NextScheduled time.Time
}

// settingsTab is the data of the admin-only "Settings" tab.
type settingsTab struct {
	stackHeader
	Repo        repoForm
	Permissions []permRow
	Schedules   []scheduleRow
	// NewSchedule holds the input of the "add schedule" form after an error.
	NewSchedule newSchedule
	TimeZone    string // the manager's time zone, in which schedules run
}

// permRow is one user in the permission table of a stack (admin view). It
// is also the data of the "perm-row" partial, so it carries what the
// partial needs on its own.
type permRow struct {
	StackName string
	CSRF      string
	User      store.User
	Perm      store.Permission
}

// loadStack resolves the stack in the URL and checks that the user may
// perform action on it. Stacks the user may not see answer 404, so their
// names are not revealed; visible stacks with a missing right answer 403.
// On failure the response has been written and ok is false.
func (s *Server) loadStack(w http.ResponseWriter, r *http.Request, action auth.Action) (st store.Stack, perm store.Permission, ok bool) {
	a := authFrom(r.Context())
	name := chi.URLParam(r, "name")
	// Only names that discovery could have produced; the database lookup
	// below then restricts it to stacks that were actually discovered.
	if !stacks.ValidName(name) {
		s.renderError(w, r, http.StatusNotFound, "Stack not found.")
		return st, perm, false
	}
	st, err := s.Store.StackByName(r.Context(), name)
	if errors.Is(err, store.ErrNotFound) {
		s.renderError(w, r, http.StatusNotFound, "Stack not found.")
		return st, perm, false
	}
	if err != nil {
		s.internalError(w, r, "loading stack", err)
		return st, perm, false
	}
	if perm, err = s.Store.Permission(r.Context(), a.User.ID, st.ID); err != nil {
		s.internalError(w, r, "loading permission", err)
		return st, perm, false
	}
	if !auth.Allowed(a.User, perm, auth.ActionView) {
		s.renderError(w, r, http.StatusNotFound, "Stack not found.")
		return st, perm, false
	}
	if !auth.Allowed(a.User, perm, action) {
		s.renderError(w, r, http.StatusForbidden, "You are not allowed to do this.")
		return st, perm, false
	}
	return st, perm, true
}

// stackBackups shows the Backups tab.
func (s *Server) stackBackups(w http.ResponseWriter, r *http.Request) {
	st, perm, ok := s.loadStack(w, r, auth.ActionView)
	if !ok {
		return
	}
	page, err := s.loadBackupsTab(r, st, perm)
	if err != nil {
		s.internalError(w, r, "loading stack", err)
		return
	}
	s.render(w, r, http.StatusOK, "stack.html", pageData{Title: st.Name + " backups", Data: page})
}

// loadBackupsTab collects the data of the Backups tab.
func (s *Server) loadBackupsTab(r *http.Request, st store.Stack, perm store.Permission) (backupsTab, error) {
	a := authFrom(r.Context())
	page := backupsTab{
		stackHeader: s.newStackHeader(a.User, perm, st, tabBackups),
		CanRestore:  auth.Allowed(a.User, perm, auth.ActionRestore),
		CSRF:        a.Session.CSRFToken,
	}
	scheds, err := s.scheduleRows(r.Context(), st)
	if err != nil {
		return page, err
	}
	page.NextScheduled = nextScheduled(scheds, jobs.TypeBackup)
	repo, err := s.Store.RepositoryInfoForStack(r.Context(), st.ID)
	if errors.Is(err, store.ErrNotFound) {
		return page, nil
	}
	if err != nil {
		return page, err
	}
	page.Repo = &repo
	if page.Snapshots, err = s.Store.StackSnapshots(r.Context(), repo.ID, st.Name, s.Hostname); err != nil {
		return page, err
	}
	all, err := s.Store.ListSnapshots(r.Context(), repo.ID)
	page.Host, page.OtherSnapshots = s.Hostname, len(all)-len(page.Snapshots)
	return page, err
}

// refreshCooldown is the minimum time between two refreshes of a stack's
// snapshot list from the UI, so repeated clicks do not start a restic
// process each.
const refreshCooldown = 10 * time.Second

// refreshStackSnapshots refreshes the snapshot list of a stack (read-only
// restic). Everyone who may view the stack may do this. HTMX requests get
// the snapshot section back, plain posts a redirect to the Backups tab.
func (s *Server) refreshStackSnapshots(w http.ResponseWriter, r *http.Request) {
	st, perm, ok := s.loadStack(w, r, auth.ActionView)
	if !ok {
		return
	}
	repo, err := s.Store.RepositoryInfoForStack(r.Context(), st.ID)
	if errors.Is(err, store.ErrNotFound) {
		s.renderError(w, r, http.StatusBadRequest, "This stack has no repository.")
		return
	}
	if err != nil {
		s.internalError(w, r, "loading repository", err)
		return
	}

	var info, errMsg string
	if age := s.now().Sub(repo.SnapshotsRefreshedAt); age >= 0 && age < refreshCooldown {
		info = "The list was refreshed just now."
	} else {
		s.audit(r, store.AuditEntry{UserID: authFrom(r.Context()).User.ID, Action: "snapshots.refresh", StackID: st.ID})
		n, err := s.refreshSnapshots(r.Context(), st)
		if err != nil {
			errMsg = "Refresh failed: " + err.Error()
		} else {
			info = fmt.Sprintf("Refreshed: %s in the repository.", plural(n, "snapshot"))
		}
	}

	if !isHTMX(r) {
		http.Redirect(w, r, "/stacks/"+url.PathEscape(st.Name)+"/"+tabBackups, http.StatusSeeOther)
		return
	}
	page, err := s.loadBackupsTab(r, st, perm)
	if err != nil {
		s.internalError(w, r, "loading stack", err)
		return
	}
	page.RefreshInfo, page.RefreshError = info, errMsg
	s.renderPartial(w, "stack.html", "snapshots", page)
}

// stackSettings shows the admin-only settings of a stack: repository,
// schedules, enable/disable and the per-user permissions.
func (s *Server) stackSettings(w http.ResponseWriter, r *http.Request) {
	st, _, ok := s.loadStack(w, r, auth.ActionView)
	if !ok {
		return
	}
	s.renderSettings(w, r, st, settingsView{})
}

// settingsView holds the per-request parts of the Settings tab.
type settingsView struct {
	Status      int // HTTP status; 0 = 200
	Info, Error string
	// Location, if set, replaces the stored location in the form (to keep
	// the user's input after an error).
	Location string
	// OfferInit shows the "Initialize repository" button (restic reported
	// that no repository exists at the stored location).
	OfferInit bool
	// NewSchedule keeps the input of the "add schedule" form after an
	// error.
	NewSchedule newSchedule
}

// newSchedule is the input of the "add schedule" form.
type newSchedule struct {
	CronExpr        string
	Update          bool // job type update (default: backup)
	PreStop         bool
	RollbackInPlace bool
}

// renderSettings renders the Settings tab.
func (s *Server) renderSettings(w http.ResponseWriter, r *http.Request, st store.Stack, v settingsView) {
	repo, err := s.loadRepoForm(r.Context(), st)
	if err != nil {
		s.internalError(w, r, "loading repository", err)
		return
	}
	if v.Location != "" {
		repo.Location = v.Location
	}
	repo.OfferInit = v.OfferInit && repo.Exists
	if v.Status == 0 {
		v.Status = http.StatusOK
	}
	rows, err := s.permissionRows(r, st)
	if err != nil {
		s.internalError(w, r, "listing permissions", err)
		return
	}
	scheds, err := s.scheduleRows(r.Context(), st)
	if err != nil {
		s.internalError(w, r, "listing schedules", err)
		return
	}
	page := settingsTab{
		stackHeader: s.newStackHeader(authFrom(r.Context()).User, store.Permission{}, st, tabSettings), // admin-only tab: no grant needed
		Repo:        repo,
		Permissions: rows,
		Schedules:   scheds,
		TimeZone:    s.now().Local().Format("MST (-07:00)"),
	}
	page.NewSchedule = v.NewSchedule
	s.render(w, r, v.Status, "stack_settings.html",
		pageData{Title: st.Name + " settings", Info: v.Info, Error: v.Error, Data: page})
}

// permissionRows lists all non-admin users with their permission on st.
// Admins are left out: they may do everything anyway.
func (s *Server) permissionRows(r *http.Request, st store.Stack) ([]permRow, error) {
	users, err := s.Store.ListUsers(r.Context())
	if err != nil {
		return nil, err
	}
	perms, err := s.Store.PermissionsForStack(r.Context(), st.ID)
	if err != nil {
		return nil, err
	}
	csrf := authFrom(r.Context()).Session.CSRFToken
	var rows []permRow
	for _, u := range users {
		if u.IsAdmin {
			continue
		}
		p, ok := perms[u.ID]
		if !ok {
			p = store.Permission{UserID: u.ID, StackID: st.ID}
		}
		rows = append(rows, permRow{StackName: st.Name, CSRF: csrf, User: u, Perm: p})
	}
	return rows, nil
}

// setPermission changes one user's permission on a stack (admin only).
// HTMX requests get the updated table row back, plain form posts a
// redirect to the stack page.
func (s *Server) setPermission(w http.ResponseWriter, r *http.Request) {
	st, _, ok := s.loadStack(w, r, auth.ActionView)
	if !ok {
		return
	}
	a := authFrom(r.Context())
	userID, err := strconv.ParseInt(r.PostFormValue("user_id"), 10, 64)
	if err != nil {
		s.renderError(w, r, http.StatusBadRequest, "Invalid user.")
		return
	}
	user, err := s.Store.UserByID(r.Context(), userID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && user.IsAdmin) {
		s.renderError(w, r, http.StatusBadRequest, "Invalid user.")
		return
	}
	if err != nil {
		s.internalError(w, r, "loading user", err)
		return
	}
	p := store.Permission{
		UserID: user.ID, StackID: st.ID,
		CanBackup:  r.PostFormValue("can_backup") == "on",
		CanRestore: r.PostFormValue("can_restore") == "on",
		CanLogs:    r.PostFormValue("can_logs") == "on",
		CanUpdate:  r.PostFormValue("can_update") == "on",
	}
	if err := s.Store.SetPermission(r.Context(), p); err != nil {
		s.internalError(w, r, "saving permission", err)
		return
	}
	s.audit(r, store.AuditEntry{UserID: a.User.ID, Action: "permission.update", StackID: st.ID,
		Detail: fmt.Sprintf("user %q: backup=%t restore=%t update=%t logs=%t",
			user.Username, p.CanBackup, p.CanRestore, p.CanUpdate, p.CanLogs)})

	if isHTMX(r) {
		s.renderPartial(w, "stack_settings.html", "perm-row",
			permRow{StackName: st.Name, CSRF: a.Session.CSRFToken, User: user, Perm: p})
		return
	}
	http.Redirect(w, r, settingsURL(st), http.StatusSeeOther) //nolint:gosec // G710: same-site path, see settingsURL
}

// setStackEnabled enables or disables a stack (admin only). Disabled
// stacks are skipped by the scheduler; manual jobs stay possible.
func (s *Server) setStackEnabled(w http.ResponseWriter, r *http.Request) {
	st, _, ok := s.loadStack(w, r, auth.ActionView)
	if !ok {
		return
	}
	enabled := r.PostFormValue("enabled") == "true"
	if err := s.Store.SetStackEnabled(r.Context(), st.ID, enabled); err != nil {
		s.internalError(w, r, "updating stack", err)
		return
	}
	action := "stack.disable"
	if enabled {
		action = "stack.enable"
	}
	s.audit(r, store.AuditEntry{UserID: authFrom(r.Context()).User.ID, Action: action, StackID: st.ID})
	http.Redirect(w, r, settingsURL(st), http.StatusSeeOther) //nolint:gosec // G710: same-site path, see settingsURL
}

// requireAdmin answers 403 for non-admins.
func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a := authFrom(r.Context()); a == nil || !a.User.IsAdmin {
			s.renderError(w, r, http.StatusForbidden, "Only administrators can do this.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// internalError logs err and shows a generic error page.
func (s *Server) internalError(w http.ResponseWriter, r *http.Request, doing string, err error) {
	s.Logger.Error(doing, "path", r.URL.Path, "error", err)
	s.renderError(w, r, http.StatusInternalServerError, "Internal error.")
}

// settingsURL is the path of a stack's Settings tab. It always starts with
// "/stacks/" and the name (a discovered stack name) is path-escaped, so it
// can never point to another site.
func settingsURL(st store.Stack) string {
	return "/stacks/" + url.PathEscape(st.Name) + "/" + tabSettings
}
