// Package web contains the manager's HTTP handlers, templates and static
// assets (server-rendered html/template + HTMX, no build step).
package web

import (
	"context"
	"html/template"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/MB512/stackvault/internal/auth"
	"github.com/MB512/stackvault/internal/store"
)

// Server holds the dependencies of the HTTP handlers.
type Server struct {
	Store  *store.Store
	Logger *slog.Logger
	// SessionTTL is the (sliding) session lifetime.
	SessionTTL time.Duration
	// SecureCookies marks cookies Secure and requires HTTPS for login. Only
	// false with STACKVAULT_ALLOW_INSECURE_HTTP.
	SecureCookies bool
	Limiter       *auth.LoginLimiter
	// Snapshots refreshes snapshot caches (read-only restic). If nil,
	// refreshing is reported as unavailable.
	Snapshots SnapshotRefresher
	// Repos initializes new repositories (restic init). If nil, the
	// action is reported as unavailable.
	Repos RepoInitializer
	// Now returns the current time; defaults to time.Now (tests override it).
	Now func() time.Time

	templates map[string]*template.Template
}

func (s *Server) now() time.Time {
	if s.Now == nil {
		return time.Now()
	}
	return s.Now()
}

// Handler returns the root HTTP handler with all routes and middleware.
func (s *Server) Handler() (http.Handler, error) {
	t, err := parseTemplates()
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
	r.Handle("/static/*", staticHandler())

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
			r.Get("/stacks/{name}", s.stackDetail)
			r.Post("/stacks/{name}/snapshots/refresh", s.refreshStackSnapshots)

			// Admin only.
			r.Group(func(r chi.Router) {
				r.Use(s.requireAdmin)
				r.Get("/stacks/{name}/settings", s.stackSettings)
				r.Post("/stacks/{name}/repository", s.saveRepository)
				r.Post("/stacks/{name}/repository/init", s.initRepository)
				r.Post("/stacks/{name}/permissions", s.setPermission)
				r.Post("/stacks/{name}/enabled", s.setStackEnabled)
				r.Get("/admin/users", s.userList)
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
