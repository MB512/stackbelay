package web

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"time"

	"github.com/MB512/stackvault/internal/buildinfo"
	"github.com/MB512/stackvault/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// pages are the page templates; each is combined with base.html and
// partials.html.
var pages = []string{"login.html", "password.html", "error.html", "stacks.html", "stack.html", "stack_settings.html", "users.html", "job.html", "restore.html", "stack_logs.html", "update.html"}

// templateFuncs are available in all templates.
var templateFuncs = template.FuncMap{
	// logTail is how many lines per container the Logs tab starts with.
	"logTail": func() int { return logTailLines },
	// version is the build version, shown in the page footer.
	"version": func() string { return buildinfo.Version },
	// when formats a timestamp for display; the zero time is shown as "never".
	"when": func(t time.Time) string {
		if t.IsZero() {
			return "never"
		}
		return t.UTC().Format("2006-01-02 15:04 UTC")
	},
}

// parseTemplates parses every page together with the base layout.
// html/template escapes all values by context (HTML, attributes, URLs), so
// user-controlled strings cannot inject markup.
func parseTemplates() (map[string]*template.Template, error) {
	out := map[string]*template.Template{}
	for _, p := range pages {
		t, err := template.New(p).Funcs(templateFuncs).ParseFS(templateFS,
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
}

// render executes a page into a buffer first, so a template error never
// produces a half-written response.
func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, page string, d pageData) {
	if a := authFrom(r.Context()); a != nil {
		d.User, d.CSRF = &a.User, a.Session.CSRFToken
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
