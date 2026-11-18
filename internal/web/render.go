package web

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/MB512/stackvault/internal/buildinfo"
	"github.com/MB512/stackvault/internal/docker"
	"github.com/MB512/stackvault/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// pages are the page templates; each is combined with base.html and
// partials.html.
var pages = []string{"login.html", "password.html", "error.html", "stacks.html", "stack_status.html", "stack.html", "stack_settings.html", "users.html", "job.html", "restore.html", "stack_logs.html", "stack_update.html", "stack_compose.html", "stack_shell.html", "admin_jobs.html", "admin_audit.html", "logs.html", "admin_host.html"}

// templateFuncs returns the functions available in all templates. Times
// are shown in the manager's time zone (s.loc).
func (s *Server) templateFuncs() template.FuncMap {
	loc := s.loc()
	return template.FuncMap{
		// version is the build version, shown in the page footer.
		"version": func() string { return buildinfo.Version },
		// imageVersion formats an image version for the update check: the
		// version label if the image has one, else build date and short digest.
		"imageVersion": func(v docker.ImageVersion) string { return imageVersion(v, loc) },
		// when formats a timestamp for display with its zone abbreviation;
		// the zero time is shown as "never".
		"when": func(t time.Time) string {
			if t.IsZero() {
				return "never"
			}
			return t.In(loc).Format("2006-01-02 15:04 MST")
		},
		// timeZone names the zone times are shown in, e.g. "Europe/Berlin".
		"timeZone": s.zoneName,
		// bytes, percent and seconds format job progress.
		"bytes": formatBytes,
		// size formats a signed byte count (image sizes), negative as 0.
		"size":    func(n int64) string { return formatBytes(uint64(max(n, 0))) },
		"percent": func(f float64) string { return fmt.Sprintf("%.0f %%", f*100) },
		"seconds": formatSeconds,
	}
}

// parseTemplates parses every page together with the base layout.
// html/template escapes all values by context (HTML, attributes, URLs), so
// user-controlled strings cannot inject markup.
func parseTemplates(funcs template.FuncMap) (map[string]*template.Template, error) {
	out := map[string]*template.Template{}
	for _, p := range pages {
		t, err := template.New(p).Funcs(funcs).ParseFS(templateFS,
			"templates/base.html", "templates/partials.html", "templates/"+p)
		if err != nil {
			return nil, fmt.Errorf("parsing template %s: %w", p, err)
		}
		out[p] = t
	}
	return out, nil
}

// pageData is passed to every template.
type pageData struct {
	Title string
	User  *store.User // nil when not logged in
	CSRF  string
	Error string
	Info  string
	Data  any // page-specific
	// GlobalLogs shows the "Logs" link in the header (the user has the
	// logs right on some stack); set by render.
	GlobalLogs bool
}

// render executes a page into a buffer first, so a template error never
// produces a half-written response.
func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, page string, d pageData) {
	if a := authFrom(r.Context()); a != nil {
		d.User, d.CSRF = &a.User, a.Session.CSRFToken
		d.GlobalLogs = s.mayViewSomeLogs(r.Context(), a.User)
	}
	t, ok := s.templates[page]
	if !ok {
		s.Logger.Error("unknown template", "page", page)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "base", d); err != nil {
		s.Logger.Error("rendering template", "page", page, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store") // pages contain CSRF tokens and user data
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// renderPartial executes one named template of a page (an HTML fragment
// for an HTMX request) with data as its dot.
func (s *Server) renderPartial(w http.ResponseWriter, page, name string, data any) {
	t, ok := s.templates[page]
	if !ok {
		s.Logger.Error("unknown template", "page", page)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, name, data); err != nil {
		s.Logger.Error("rendering partial", "page", page, "name", name, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(buf.Bytes())
}

// isHTMX reports whether the request was sent by HTMX (and expects an HTML
// fragment instead of a full page or redirect).
func isHTMX(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

// renderError shows a simple error page.
func (s *Server) renderError(w http.ResponseWriter, r *http.Request, status int, msg string) {
	s.render(w, r, status, "error.html", pageData{Title: http.StatusText(status), Error: msg})
}

// staticHandler serves the embedded static assets under /static/.
func staticHandler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err) // the embedded directory always exists
	}
	files := http.StripPrefix("/static/", http.FileServerFS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		files.ServeHTTP(w, r)
	})
}

// imageVersion formats v for display: "1.2.3 (2026-01-02, sha256:abcdef12)",
// the date in loc. Unknown parts are left out; nothing known gives "".
func imageVersion(v docker.ImageVersion, loc *time.Location) string {
	var parts []string
	if !v.Created.IsZero() {
		parts = append(parts, v.Created.In(loc).Format("2006-01-02"))
	}
	if d := strings.TrimPrefix(v.Digest, "sha256:"); d != "" {
		parts = append(parts, "sha256:"+d[:min(len(d), 12)])
	}
	switch {
	case v.Version != "" && len(parts) > 0:
		return v.Version + " (" + strings.Join(parts, ", ") + ")"
	case v.Version != "":
		return v.Version
	default:
		return strings.Join(parts, ", ")
	}
}

// formatBytes shows a size with a binary unit: "512 B", "1.5 MiB".
func formatBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit && exp < 5; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// formatSeconds shows a duration roughly: "45 s", "3 min", "2 h 5 min".
func formatSeconds(s int64) string {
	switch {
	case s < 60:
		return fmt.Sprintf("%d s", s)
	case s < 3600:
		return fmt.Sprintf("%d min", (s+30)/60)
	default:
		return fmt.Sprintf("%d h %d min", s/3600, s%3600/60)
	}
}
