// Package web contains the manager's HTTP handlers, templates and static
// assets (server-rendered html/template + HTMX, no build step).
package web

import (
	"context"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/MB512/stackbelay/internal/auth"
	"github.com/MB512/stackbelay/internal/jobs"
	"github.com/MB512/stackbelay/internal/store"
)

// Server holds the dependencies of the HTTP handlers.
type Server struct {
	Store  *store.Store
	Logger *slog.Logger
	// SessionTTL is the (sliding) session lifetime.
	SessionTTL time.Duration
	// SecureCookies marks cookies Secure and requires HTTPS for login. Only
	// false with STACKBELAY_ALLOW_INSECURE_HTTP.
	SecureCookies bool
	Limiter       *auth.LoginLimiter
	// Snapshots refreshes snapshot caches (read-only restic). If nil,
	// refreshing is reported as unavailable.
	Snapshots SnapshotRefresher
	// Repos initializes new repositories (restic init). If nil, the
	// action is reported as unavailable.
	Repos RepoInitializer
	// Jobs queues and cancels backups and restores. If nil, jobs are
	// unavailable and JobsDisabledReason says why.
	Jobs               JobQueue
	JobsDisabledReason string
	// MaxJobs is STACKBELAY_MAX_JOBS (jobs running at the same time), for
	// explaining why a job waits; 0 if unknown.
	MaxJobs int
	// Schedules is told when schedules change. nil: schedules can still be
	// edited, they take effect when the manager starts.
	Schedules ScheduleReloader
	// JobsDir is the jobs directory (for job logs).
	JobsDir string
	// JobLogRetention: job directories of jobs finished longer ago are
	// deleted by the dispatcher (issue #16); the job page says so. 0 = kept.
	JobLogRetention time.Duration
	// StacksRoot is STACKS_ROOT; stack directories are looked up there by
	// discovery (stacks.Lookup), never taken from the URL.
	StacksRoot string
	// Containers lists a stack's containers for the Status tab. If nil, the
	// tab says that the list is unavailable.
	Containers ContainerLister
	// Logs reads container logs for the Logs tab. If nil, the tab says
	// that logs are unavailable.
	Logs StackLogs
	// Compose validates edited compose files. If nil, the compose editor
	// cannot apply changes.
	Compose ComposeValidator
	// EnableShell turns on the admin-only "Shell" tab
	// (STACKBELAY_ENABLE_SHELL). When false its routes do not exist.
	EnableShell bool
	// Shells opens shells in stack containers. Required with EnableShell.
	Shells ShellStarter
	// Updates checks for newer images (Update tab). If nil, the tab offers
	// no check.
	Updates UpdateChecker
	// Cleanup lists what a host cleanup would remove (admin Host tab,
	// read-only; the removal runs in a worker job). If nil, there is no
	// preview.
	Cleanup jobs.CleanupLister
	// CleanupImageMinAge is STACKBELAY_CLEANUP_IMAGE_MIN_AGE (preview rules).
	CleanupImageMinAge time.Duration
	// RepoSnapshots lists the snapshots of a repository that is not saved
	// yet, for "Import stack" (read-only restic). If nil, imports are not
	// available.
	RepoSnapshots RepoSnapshotLister
	// Stopping is closed when the manager shuts down; open live streams
	// then end at once instead of delaying the shutdown (clients
	// reconnect). nil: streams only end with their client.
	Stopping <-chan struct{}
	// StreamInterval is how often live streams look for news; default 500ms.
	StreamInterval time.Duration
	// OwnStack is the stack the manager itself belongs to ("" if none).
	// It cannot be stopped from the UI: nobody could start it again there.
	OwnStack string
	// Hostname is this host's restic --host (STACKBELAY_HOSTNAME). Only
	// snapshots of a stack made on this host are listed and restorable.
	// Empty: snapshots of all hosts.
	Hostname string
	// Now returns the current time; defaults to time.Now (tests override it).
	Now func() time.Time
	// Location is the time zone all times are shown in (the manager's TZ);
	// nil means time.Local.
	Location *time.Location

	templates  map[string]*template.Template
	logStreams streamLimiter  // open log streams
	shells     streamLimiter  // open shell sessions
	imports    pendingImports // "Import stack" between its two steps
	diskSizes  diskSizes      // "Calculate" of stack sizes on disk
	// running update checks (not streams, but limited the same way)
	updateChecks streamLimiter
	// the last update check per stack
	updateResults updateResults
	// the State column of the stack list, cached per stack
	stackStates stateCache
	shellCheck  time.Duration // session re-check of open shells; default shellSessionCheck (tests shorten it)
}

// loc is the time zone for displayed times.
func (s *Server) loc() *time.Location {
	if s.Location == nil {
		return time.Local
	}
	return s.Location
}

// zoneName names the display time zone, e.g. "Europe/Berlin"; for an
// unnamed zone (time.Local without TZ) its abbreviation at the current
// time (e.g. "UTC", or "+0300" if the zone has none).
func (s *Server) zoneName() string {
	if name := s.loc().String(); name != "" && name != "Local" {
		return name
	}
	return s.now().In(s.loc()).Format("MST")
}

func (s *Server) now() time.Time {
	if s.Now == nil {
		return time.Now()
	}
	return s.Now()
}

// Handler returns the root HTTP handler with all routes and middleware.
func (s *Server) Handler() (http.Handler, error) {
	assets, err := assetVersions()
	if err != nil {
		return nil, fmt.Errorf("reading static assets: %w", err)
	}
	t, err := parseTemplates(s.templateFuncs(assets))
	if err != nil {
		return nil, err
	}
	s.templates = t
	if s.Limiter == nil {
		s.Limiter = &auth.LoginLimiter{}
	}

	r := chi.NewRouter()
	r.Use(middleware.Recoverer) // a panicking handler returns 500 instead of killing the process
	r.Use(s.logRequests)
	r.Use(securityHeaders)

	r.Get("/healthz", s.healthz)
	r.Handle("/static/*", staticHandler(assets))

	r.Group(func(r chi.Router) {
		// Reject cross-site browser requests that change state (first CSRF
		// layer, based on Sec-Fetch-Site/Origin; the per-session token in
		// checkCSRF is the second).
		r.Use(http.NewCrossOriginProtection().Handler)
		r.Use(s.loadSession)

		r.Get("/login", s.loginForm)
		r.Post("/login", s.login)

		r.Group(func(r chi.Router) {
			r.Use(s.requireUser)
			r.Use(s.checkCSRF)
			r.Use(s.requirePasswordChanged)

			r.Post("/logout", s.logout)
			r.Get("/account/password", s.passwordForm)
			r.Post("/account/password", s.changePassword)

			r.Get("/", s.stackList)
			r.Get("/stacks/{name}", s.stackStatus)
			r.Get("/stacks/{name}/status/live", s.stackStatusLive)
			r.Get("/stacks/{name}/state", s.stackStateCell)
			r.Get("/stacks/{name}/backups", s.stackBackups)
			r.Post("/stacks/{name}/snapshots/refresh", s.refreshStackSnapshots)
			r.Post("/stacks/{name}/size", s.calculateStackSize)
			r.Post("/stacks/{name}/backup", s.startBackup)
			r.Post("/stacks/{name}/stop", s.startControl(jobs.TypeStop))
			r.Post("/stacks/{name}/start", s.startControl(jobs.TypeStart))
			r.Post("/stacks/{name}/restart", s.startControl(jobs.TypeRestart))
			r.Post("/stacks/{name}/up", s.startControl(jobs.TypeUp))
			r.Post("/stacks/{name}/down", s.startControl(jobs.TypeDown))
			r.Get("/stacks/{name}/logs", s.stackLogsPage)
			r.Get("/stacks/{name}/logs/events", s.stackLogEvents)
			r.Get("/stacks/{name}/restore", s.restoreForm)
			r.Post("/stacks/{name}/restore", s.startRestore)
			r.Get("/stacks/{name}/update", s.stackUpdate)
			r.Post("/stacks/{name}/update", s.startUpdate)
			r.Post("/stacks/{name}/update/check", s.checkUpdates)
			r.Get("/logs", s.globalLogs)
			r.Get("/logs/events", s.globalLogEvents)
			r.Get("/jobs/{id}", s.jobDetail)
			r.Get("/jobs/{id}/events", s.jobEvents)
			r.Post("/jobs/{id}/cancel", s.cancelJob)

			// Admin only.
			r.Group(func(r chi.Router) {
				r.Use(s.requireAdmin)
				if s.EnableShell {
					r.Get("/stacks/{name}/shell", s.stackShell)
					r.Get("/stacks/{name}/shell/ws", s.shellSocket)
				}
				r.Get("/stacks/{name}/compose", s.stackCompose)
				r.Post("/stacks/{name}/compose", s.applyCompose)
				r.Get("/stacks/{name}/settings", s.stackSettings)
				r.Post("/stacks/{name}/repository", s.setStackRepository)
				r.Post("/stacks/{name}/permissions", s.setPermission)
				r.Post("/stacks/{name}/schedules/enabled", s.setAllSchedulesEnabled)
				r.Post("/stacks/{name}/source-host", s.setSourceHost)
				r.Post("/stacks/{name}/schedules", s.createSchedule)
				r.Post("/stacks/{name}/schedules/{id}/enabled", s.setScheduleEnabled)
				r.Post("/stacks/{name}/schedules/{id}/delete", s.deleteSchedule)
				r.Get("/admin/users", s.userList)
				r.Get("/admin/jobs", s.adminJobs)
				r.Get("/admin/jobs/live", s.adminJobsLiveFragment)
				r.Get("/admin/audit", s.adminAudit)
				r.Get("/admin/repositories", s.adminRepositories)
				r.Post("/admin/repositories", s.createRepository)
				r.Get("/admin/repositories/{id}", s.repositoryPage)
				r.Post("/admin/repositories/{id}", s.updateRepository)
				r.Post("/admin/repositories/{id}/init", s.initRepository)
				r.Post("/admin/repositories/{id}/delete", s.deleteRepository)
				r.Get("/admin/import", s.adminImport)
				r.Post("/admin/import", s.listImport)
				r.Post("/admin/import/confirm", s.confirmImport)
				r.Get("/admin/host", s.adminHost)
				r.Post("/admin/host/cleanup", s.startCleanup)
				r.Post("/admin/host/schedules", s.createHostSchedule)
				r.Post("/admin/host/schedules/{id}/enabled", s.setHostScheduleEnabled)
				r.Post("/admin/host/schedules/{id}/delete", s.deleteHostSchedule)
				r.Post("/admin/users", s.createUser)
				r.Post("/admin/users/{id}", s.updateUser)
				r.Post("/admin/users/{id}/password", s.resetPassword)
			})
		})
	})

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		s.renderError(w, r, http.StatusNotFound, "Page not found.")
	})
	return r, nil
}

// healthz reports whether the manager and its database are usable.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.Store.Ping(ctx); err != nil {
		s.Logger.Error("health check: database not reachable", "error", err)
		http.Error(w, "database not reachable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}

// securityHeaders sets headers that are correct for every response.
// The CSP allows only same-origin resources: all assets (HTMX, CSS) are
// vendored and served by us, and there are no inline scripts or styles.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// logRequests logs one line per request (method, path, status, duration).
// Query strings are not logged, they could contain tokens.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		if (r.URL.Path == "/healthz" && ww.Status() == http.StatusOK) || strings.HasPrefix(r.URL.Path, "/static/") {
			return // keep the log readable
		}
		s.Logger.Info("http request", "method", r.Method, "path", r.URL.Path,
			"status", ww.Status(), "duration_ms", time.Since(start).Milliseconds())
	})
}
