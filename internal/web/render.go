package web

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/MB512/stackbelay/internal/buildinfo"
	"github.com/MB512/stackbelay/internal/docker"
	"github.com/MB512/stackbelay/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// pages are the page templates; each is combined with base.html and
// partials.html.
var pages = []string{"login.html", "password.html", "error.html", "stacks.html", "stack_status.html", "stack.html", "stack_settings.html", "users.html", "job.html", "restore.html", "stack_logs.html", "stack_update.html", "stack_compose.html", "stack_shell.html", "admin_jobs.html", "admin_audit.html", "logs.html", "admin_host.html", "admin_import.html", "admin_repositories.html", "admin_repository.html", "admin_notifications.html"}

// templateFuncs returns the functions available in all templates. Times
// are shown in the manager's time zone (s.loc).
func (s *Server) templateFuncs(assets map[string]string) template.FuncMap {
	loc := s.loc()
	return template.FuncMap{
		// asset is the URL of a static file with its content version
		// ("/static/style.css?v=1a2b3c4d5e6f", issue #20), so a browser
		// loads the new file right after an upgrade.
		"asset": func(name string) (string, error) {
			v, ok := assets[name]
			if !ok {
				return "", fmt.Errorf("unknown static asset %q", name)
			}
			return "/static/" + name + "?v=" + v, nil
		},
		// envRowData and emptyEnvRow feed the "env-row" partial of the
		// repository form (a row plus the key choices).
		"envRowData":  func(row envRow, keys []envKey) envRowData { return envRowData{Row: row, Keys: keys} },
		"emptyEnvRow": func() envRow { return envRow{} },
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
		// ago and in show the time since or until t roughly ("4 h", issue
		// #37); the full time belongs in a tooltip (when).
		"ago": func(t time.Time) string { return formatAge(s.now().Sub(t)) },
		"in":  func(t time.Time) string { return "in " + formatAge(t.Sub(s.now())) },
		// timeZone names the zone times are shown in, e.g. "Europe/Berlin".
		"timeZone": s.zoneName,
		// bytes, percent and seconds format job progress.
		"bytes": formatBytes,
		// size formats a signed byte count (image sizes), negative as 0.
		"size":    func(n int64) string { return formatBytes(uint64(max(n, 0))) },
		"percent": func(f float64) string { return fmt.Sprintf("%.0f %%", f*100) },
		"seconds": formatSeconds,
		// duration formats a time.Duration, e.g. "1 min 30 s".
		"duration": formatDuration,
		// jobDuration is how long a job ran ("running for …" while it runs,
		// "" if it never started).
		"jobDuration": func(j store.Job) string { return jobDuration(j, s.now()) },
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
	// Mute is the admin's notification mute for the header (issue #50);
	// set by render.
	Mute muteBadge
}

// render executes a page into a buffer first, so a template error never
// produces a half-written response.
func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, page string, d pageData) {
	if a := authFrom(r.Context()); a != nil {
		d.User, d.CSRF = &a.User, a.Session.CSRFToken
		d.GlobalLogs = s.mayViewSomeLogs(r.Context(), a.User)
		d.Mute = s.muteBadge(r, a.User)
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

// assetVersions returns a short hash of the content of every embedded
// static file, by name ("style.css"). It is computed once at startup.
func assetVersions() (map[string]string, error) {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	err = fs.WalkDir(sub, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(sub, name)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		out[name] = hex.EncodeToString(sum[:6])
		return nil
	})
	return out, err
}

// staticHandler serves the embedded static assets under /static/. A URL
// with the file's current version (see "asset") may be cached for a year;
// any other is revalidated on every use (ETag = version), so a browser
// never keeps an old script or style sheet after an upgrade (issue #20).
func staticHandler(versions map[string]string) http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err) // the embedded directory always exists
	}
	files := http.StripPrefix("/static/", http.FileServerFS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if v, ok := versions[strings.TrimPrefix(r.URL.Path, "/static/")]; ok {
			// FileServer answers If-None-Match with 304 using this ETag.
			w.Header().Set("ETag", `"`+v+`"`)
			if r.URL.Query().Get("v") == v {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				w.Header().Set("Cache-Control", "no-cache")
			}
		}
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

// jobDuration describes how long j ran: finished - started, "running for
// …" while it runs, "" if it never started (queued, or cancelled while
// queued).
func jobDuration(j store.Job, now time.Time) string {
	switch {
	case j.StartedAt.IsZero():
		return ""
	case j.FinishedAt.IsZero():
		return "running for " + formatDuration(now.Sub(j.StartedAt))
	}
	return formatDuration(j.FinishedAt.Sub(j.StartedAt))
}

// formatDuration shows a duration to the second below an hour: "45 s",
// "1 min 30 s", "2 h 5 min"; "<1 s" for less than a second.
func formatDuration(d time.Duration) string {
	s := int64(d.Round(time.Second) / time.Second)
	switch {
	case d < time.Second:
		return "<1 s"
	case s < 60:
		return fmt.Sprintf("%d s", s)
	case s < 3600 && s%60 == 0:
		return fmt.Sprintf("%d min", s/60)
	case s < 3600:
		return fmt.Sprintf("%d min %d s", s/60, s%60)
	default:
		return fmt.Sprintf("%d h %d min", s/3600, s%3600/60)
	}
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
