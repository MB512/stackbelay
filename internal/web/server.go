// Package web contains the manager's HTTP handlers, templates and static
// assets (server-rendered html/template + HTMX, no build step).
package web

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// Pinger is what the health check needs from the store. Defining the small
// interface here, where it is used, keeps tests independent of SQLite.
type Pinger interface {
	Ping(ctx context.Context) error
}

// Server holds the dependencies of the HTTP handlers.
type Server struct {
	DB     Pinger
	Logger *slog.Logger
}

// Handler returns the root HTTP handler with all routes and middleware.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer) // a panicking handler returns 500 instead of killing the process
	r.Use(s.logRequests)
	r.Use(securityHeaders)

	r.Get("/healthz", s.healthz)
	return r
}

// healthz reports whether the manager and its database are usable.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.DB.Ping(ctx); err != nil {
		s.Logger.Error("health check: database not reachable", "error", err)
		http.Error(w, "database not reachable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}

// securityHeaders sets headers that are correct for every response.
// The CSP allows only same-origin resources: all assets (HTMX, CSS) are
// vendored and served by us, and there are no inline scripts.
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
		if r.URL.Path == "/healthz" && ww.Status() == http.StatusOK {
			return // keep the log readable; health checks run every few seconds
		}
		s.Logger.Info("http request", "method", r.Method, "path", r.URL.Path,
			"status", ww.Status(), "duration_ms", time.Since(start).Milliseconds())
	})
}
