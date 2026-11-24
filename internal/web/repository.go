package web

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/MB512/stackbelay/internal/auth"
	"github.com/MB512/stackbelay/internal/restic"
	"github.com/MB512/stackbelay/internal/store"
)

// Repositories are global (issue #39): admins manage them on the
// Repositories tab, and a stack's Settings tab only picks one. Several
// stacks may share a repository; a change applies to all of them.

// SnapshotRefresher refreshes the snapshot cache of a repository by asking
// restic (read-only). *snapshots.Cache implements it; tests use a fake.
type SnapshotRefresher interface {
	// Refresh refreshes the repository of a stack.
	Refresh(ctx context.Context, stackID int64) ([]store.CachedSnapshot, error)
	RefreshRepository(ctx context.Context, repoID int64) ([]store.CachedSnapshot, error)
}

// refreshTimeout bounds a snapshot refresh started from the UI.
const refreshTimeout = 60 * time.Second

// repoForm is the add/edit form of a repository. It never holds secret
// values, only the names of stored variables.
type repoForm struct {
	ID       int64  // 0 = new repository
	Name     string // current name, or the submitted one after an error
	Location string // current location, or the submitted one after an error
	EnvKeys  []string
	Allowed  []string // variable names that may be entered
	// OfferInit shows the "Initialize repository" button enabled.
	OfferInit bool
}

// allowedRepoEnv are the backend variables the form accepts. The
// repository password has its own field.
func allowedRepoEnv() []string {
	var out []string
	for _, k := range restic.BackendEnvVars {
		if k != "RESTIC_PASSWORD" && k != "RESTIC_PASSWORD_FILE" {
			out = append(out, k)
		}
	}
	return out
}

// parseEnvLines parses "KEY=value" lines. Empty lines and lines starting
// with "#" are skipped. Values are taken literally (no quotes, no trim).
func parseEnvLines(text string) (map[string]string, error) {
	env := map[string]string{}
	for n, line := range strings.Split(text, "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		switch {
		case !ok:
			return nil, fmt.Errorf("line %d: expected KEY=value", n+1)
		case !slices.Contains(allowedRepoEnv(), key):
			// Only the key is echoed, never the value.
			return nil, fmt.Errorf("line %d: variable %q is not supported", n+1, truncate(key, 40))
		}
		if _, dup := env[key]; dup {
			return nil, fmt.Errorf("line %d: %s is set twice", n+1, key)
		}
		env[key] = value
	}
	return env, nil
}

// repoInput is a submitted repository form, validated.
type repoInput struct {
	name, location, password string
	env                      map[string]string
	fromURL                  bool // credentials were moved from the URL into env
}

// parseRepoInput reads and validates the repository form fields: name,
// location (a REST URL may contain user:password, which is moved into the
// encrypted variables), password (empty = keep old's), env ("KEY=value"
// lines; non-empty replaces all stored variables) and clear_env. old is
// nil for a new repository. On error it returns a message for the user and
// the form values that may be shown again (never a password).
func parseRepoInput(r *http.Request, old *store.Repository) (repoInput, repoForm, string) {
	in := repoInput{name: strings.TrimSpace(r.PostFormValue("name"))}
	echo := repoForm{Name: in.name}
	location, urlEnv, err := restic.SplitRESTCredentials(strings.TrimSpace(r.PostFormValue("location")))
	if err != nil {
		return in, echo, capitalize(err.Error()) + "." // the input may contain a password: not echoed
	}
	in.location, echo.Location = location, location
	if err := store.ValidateRepositoryName(in.name); err != nil {
		return in, echo, capitalize(err.Error()) + "."
	}
	in.password = r.PostFormValue("password")
	in.env = map[string]string{}
	if old != nil {
		if in.password == "" {
			in.password = old.Password
		}
		if r.PostFormValue("clear_env") != "on" {
			maps.Copy(in.env, old.Env)
		}
	}
	if text := r.PostFormValue("env"); strings.TrimSpace(text) != "" {
		if in.env, err = parseEnvLines(text); err != nil {
			return in, echo, capitalize(err.Error()) + "."
		}
	}
	maps.Copy(in.env, urlEnv) // credentials from the URL win
	in.fromURL = len(urlEnv) > 0
	if err := store.ValidateRepository(in.location, in.password, in.env); err != nil {
		return in, echo, capitalize(err.Error()) + "."
	}
	return in, echo, ""
}

// savedInfo is the message after saving a repository.
func (in repoInput) savedInfo() string {
	info := "Repository saved."
	if in.fromURL {
		info += " The user name and password from the URL are stored encrypted as RESTIC_REST_USERNAME/RESTIC_REST_PASSWORD."
	}
	return info
}

// reposPage is the data of the admin "Repositories" tab.
type reposPage struct {
	Repos []store.RepositoryInfo
	New   repoForm
}

// repoPage is the data of one repository's admin page.
type repoPage struct {
	Repo store.RepositoryInfo
	Form repoForm
}

// pageView holds the per-request parts of a page: status, messages.
type pageView struct {
	Status      int // HTTP status; 0 = 200
	Info, Error string
	OfferInit   bool // repository pages: restic reported no repository at the location
}

func (v pageView) status() int {
	if v.Status == 0 {
		return http.StatusOK
	}
	return v.Status
}

// adminRepositories lists the repositories, with the form for a new one.
func (s *Server) adminRepositories(w http.ResponseWriter, r *http.Request) {
	s.renderRepos(w, r, repoForm{}, pageView{})
}

func (s *Server) renderRepos(w http.ResponseWriter, r *http.Request, form repoForm, v pageView) {
	repos, err := s.Store.ListRepositories(r.Context())
	if err != nil {
		s.internalError(w, r, "listing repositories", err)
		return
	}
	form.Allowed = allowedRepoEnv()
	s.render(w, r, v.status(), "admin_repositories.html",
		pageData{Title: "Repositories", Info: v.Info, Error: v.Error, Data: reposPage{Repos: repos, New: form}})
}

// createRepository adds a repository (admin only), then checks it by
// refreshing its snapshot cache.
func (s *Server) createRepository(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	in, echo, msg := parseRepoInput(r, nil)
	if msg != "" {
		s.renderRepos(w, r, echo, pageView{Status: http.StatusBadRequest, Error: msg})
		return
	}
	id, err := s.Store.CreateRepository(ctx, in.name, in.location, in.password, in.env)
	if errors.Is(err, store.ErrConflict) {
		s.renderRepos(w, r, echo, pageView{Status: http.StatusConflict, Error: "A repository with this name exists already."})
		return
	}
	if err != nil {
		s.internalError(w, r, "saving repository", err)
		return
	}
	s.audit(r, store.AuditEntry{UserID: authFrom(ctx).User.ID, Action: "repository.create",
		Detail: fmt.Sprintf("repository %q (id %d), location %q, variables %v", truncate(in.name, 100), id,
			truncate(in.location, 200), slices.Sorted(maps.Keys(in.env)))})
	s.renderRepo(w, r, id, repoForm{}, s.checkRepository(ctx, id, in.savedInfo()))
}

// loadRepo loads the repository from the URL, or answers 404.
func (s *Server) loadRepo(w http.ResponseWriter, r *http.Request) (store.Repository, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil || id <= 0 {
		s.renderError(w, r, http.StatusNotFound, "No such repository.")
		return store.Repository{}, false
	}
	repo, err := s.Store.Repository(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.renderError(w, r, http.StatusNotFound, "No such repository.")
		return store.Repository{}, false
	}
	if err != nil {
		s.internalError(w, r, "loading repository", err)
		return store.Repository{}, false
	}
	return repo, true
}

// repositoryPage shows one repository: edit form, initialize, delete.
func (s *Server) repositoryPage(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.loadRepo(w, r)
	if !ok {
		return
	}
	s.renderRepo(w, r, repo.ID, repoForm{}, pageView{})
}

// renderRepo renders a repository's page. echo, if it has a name, replaces
// the stored name and location in the form (the user's input after an
// error).
func (s *Server) renderRepo(w http.ResponseWriter, r *http.Request, id int64, echo repoForm, v pageView) {
	ctx := r.Context()
	info, err := s.Store.RepositoryInfo(ctx, id)
	if err != nil {
		s.internalError(w, r, "loading repository", err)
		return
	}
	form := repoForm{ID: id, Name: info.Name, Location: info.Location, Allowed: allowedRepoEnv(), OfferInit: v.OfferInit}
	// Only the variable names are shown. A secret that does not decrypt
	// (e.g. a conversion that failed) just shows none.
	if repo, err := s.Store.Repository(ctx, id); err == nil {
		form.EnvKeys = slices.Sorted(maps.Keys(repo.Env))
	}
	if echo.Name != "" || echo.Location != "" {
		form.Name, form.Location = echo.Name, echo.Location
	}
	s.render(w, r, v.status(), "admin_repository.html",
		pageData{Title: info.Name, Info: v.Info, Error: v.Error, Data: repoPage{Repo: info, Form: form}})
}

// updateRepository changes a repository (admin only); the change applies
// to every stack that uses it. Then it checks the repository by
// refreshing its snapshot cache.
func (s *Server) updateRepository(w http.ResponseWriter, r *http.Request) {
	old, ok := s.loadRepo(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	in, echo, msg := parseRepoInput(r, &old)
	if msg != "" {
		s.renderRepo(w, r, old.ID, echo, pageView{Status: http.StatusBadRequest, Error: msg})
		return
	}
	err := s.Store.UpdateRepository(ctx, old.ID, in.name, in.location, in.password, in.env)
	if errors.Is(err, store.ErrConflict) {
		s.renderRepo(w, r, old.ID, echo, pageView{Status: http.StatusConflict, Error: "A repository with this name exists already."})
		return
	}
	if err != nil {
		s.internalError(w, r, "saving repository", err)
		return
	}
	s.audit(r, store.AuditEntry{UserID: authFrom(ctx).User.ID, Action: "repository.update",
		Detail: fmt.Sprintf("repository %q (id %d), name %q, location %q, variables %v, password changed: %t",
			truncate(old.Name, 100), old.ID, truncate(in.name, 100), truncate(in.location, 200),
			slices.Sorted(maps.Keys(in.env)), in.password != old.Password)})
	s.renderRepo(w, r, old.ID, repoForm{}, s.checkRepository(ctx, old.ID, in.savedInfo()))
}

// deleteRepository removes a repository that no stack uses (admin only).
func (s *Server) deleteRepository(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.loadRepo(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	err := s.Store.DeleteRepository(ctx, repo.ID)
	if errors.Is(err, store.ErrInUse) {
		s.renderRepo(w, r, repo.ID, repoForm{}, pageView{Status: http.StatusConflict,
			Error: "Stacks still use this repository. Choose another repository (or none) on their Settings tab first."})
		return
	}
	if err != nil {
		s.internalError(w, r, "deleting repository", err)
		return
	}
	s.audit(r, store.AuditEntry{UserID: authFrom(ctx).User.ID, Action: "repository.delete",
		Detail: fmt.Sprintf("repository %q (id %d), location %q", truncate(repo.Name, 100), repo.ID, truncate(repo.Location, 200))})
	http.Redirect(w, r, "/admin/repositories", http.StatusSeeOther)
}

// checkRepository refreshes a repository's snapshot cache as an access
// check and describes the result. If there is no repository at the
// location, it offers to initialize one.
func (s *Server) checkRepository(ctx context.Context, repoID int64, info string) pageView {
	if s.Snapshots == nil {
		return pageView{Info: info, Error: "Checking access is not available."}
	}
	rctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
	snaps, err := s.Snapshots.RefreshRepository(rctx, repoID)
	if err != nil {
		s.Logger.Warn("refreshing snapshots", "repository_id", repoID, "error", err)
		re := asRefreshError(err)
		if re.noRepo {
			return pageView{Info: info, OfferInit: true, Error: "There is no restic repository at this location yet: " + re.msg}
		}
		return pageView{Info: info, Error: "restic could not open the repository: " + re.msg}
	}
	n := 0
	for _, sn := range snaps {
		if !sn.Missing {
			n++
		}
	}
	return pageView{Info: fmt.Sprintf("%s restic found %s.", info, plural(n, "snapshot"))}
}

// refreshError is a refresh failure prepared for display.
type refreshError struct {
	msg    string // shortened restic message, safe to show
	noRepo bool   // restic: no repository at the location
}

func (e *refreshError) Error() string { return e.msg }

// asRefreshError shortens a refresh error for display; restic output
// never contains the credentials (see restic.CLI).
func asRefreshError(err error) *refreshError {
	msg := err.Error()
	// errors.As finds a *restic.Error anywhere in the wrapped chain,
	// like catching a specific exception type (or InnerException) in C#.
	var re *restic.Error
	if errors.As(err, &re) {
		msg = re.Message()
	} else if errors.Is(err, context.DeadlineExceeded) {
		msg = "no answer within " + refreshTimeout.String()
	}
	return &refreshError{msg: truncate(msg, 300), noRepo: restic.IsNoRepository(err)}
}

// refreshSnapshots refreshes the snapshot cache of the stack's repository
// and returns the number of snapshots of this stack and host that exist
// in it. Errors are *refreshError.
func (s *Server) refreshSnapshots(ctx context.Context, st store.Stack) (int, error) {
	if s.Snapshots == nil {
		return 0, &refreshError{msg: "snapshot refresh is not available"}
	}
	ctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
	if _, err := s.Snapshots.Refresh(ctx, st.ID); err != nil {
		s.Logger.Warn("refreshing snapshots", "stack", st.Name, "error", err)
		return 0, asRefreshError(err)
	}
	repo, err := s.Store.RepositoryInfoForStack(ctx, st.ID)
	if err != nil {
		return 0, &refreshError{msg: "loading repository: " + err.Error()}
	}
	own, err := s.Store.StackSnapshots(ctx, repo.ID, st.Name, s.snapshotHost(st))
	if err != nil {
		return 0, &refreshError{msg: "listing snapshots: " + err.Error()}
	}
	n := 0
	for _, sn := range own {
		if !sn.Missing {
			n++
		}
	}
	return n, nil
}

// plural returns "1 snapshot", "2 snapshots".
func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// RepoInitializer creates a new restic repository. *restic.CLI implements
// it; tests use a fake.
type RepoInitializer interface {
	Init(ctx context.Context, repo restic.Repo) error
}

// initRepository runs "restic init" at the repository's stored location
// (admin only), then checks the new repository. The button is only
// offered after restic reported that no repository exists there, and
// restic itself refuses to init over an existing repository.
func (s *Server) initRepository(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.loadRepo(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	if s.Repos == nil {
		s.renderRepo(w, r, repo.ID, repoForm{}, pageView{Status: http.StatusServiceUnavailable,
			Error: "Initializing repositories is not available."})
		return
	}
	s.audit(r, store.AuditEntry{UserID: authFrom(ctx).User.ID, Action: "repository.init",
		Detail: fmt.Sprintf("repository %q (id %d), location %q", truncate(repo.Name, 100), repo.ID, truncate(repo.Location, 200))})
	initCtx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
	if err := s.Repos.Init(initCtx, repo.Restic()); err != nil {
		s.Logger.Warn("initializing repository", "repository", repo.Name, "error", err)
		msg := err.Error()
		var re *restic.Error
		if errors.As(err, &re) {
			msg = re.Message()
		}
		s.renderRepo(w, r, repo.ID, repoForm{}, pageView{Error: "restic could not initialize the repository: " + truncate(msg, 300)})
		return
	}
	s.renderRepo(w, r, repo.ID, repoForm{}, s.checkRepository(ctx, repo.ID, "Repository initialized."))
}

// stackRepoForm is the repository section of a stack's Settings tab.
type stackRepoForm struct {
	Exists  bool
	Current int64 // id of the stack's repository; 0 = none
	Options []store.RepositoryInfo
}

// loadStackRepoForm returns the repository section data for the Settings
// tab.
func (s *Server) loadStackRepoForm(ctx context.Context, st store.Stack) (stackRepoForm, error) {
	var f stackRepoForm
	repos, err := s.Store.ListRepositories(ctx)
	if err != nil {
		return f, err
	}
	f.Options = repos
	cur, err := s.Store.RepositoryInfoForStack(ctx, st.ID)
	switch {
	case err == nil:
		f.Exists, f.Current = true, cur.ID
	case !errors.Is(err, store.ErrNotFound):
		return f, err
	}
	return f, nil
}

// setStackRepository chooses the repository of a stack (admin only):
// form field repository, a repository id or "" for none. Then it checks
// access by refreshing the snapshot cache.
func (s *Server) setStackRepository(w http.ResponseWriter, r *http.Request) {
	st, _, ok := s.loadStack(w, r, auth.ActionView)
	if !ok {
		return
	}
	ctx := r.Context()
	var repo store.RepositoryInfo
	if v := r.PostFormValue("repository"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err == nil {
			repo, err = s.Store.RepositoryInfo(ctx, id)
		}
		if err != nil {
			s.renderSettings(w, r, st, settingsView{Status: http.StatusBadRequest, Error: "Choose a repository from the list."})
			return
		}
	}
	if err := s.Store.SetStackRepository(ctx, st.ID, repo.ID); err != nil {
		s.internalError(w, r, "setting repository", err)
		return
	}
	detail := "none"
	if repo.ID != 0 {
		detail = fmt.Sprintf("repository %q (id %d)", truncate(repo.Name, 100), repo.ID)
	}
	s.audit(r, store.AuditEntry{UserID: authFrom(ctx).User.ID, Action: "stack.repository", StackID: st.ID, Detail: detail})
	if repo.ID == 0 {
		s.renderSettings(w, r, st, settingsView{Info: "The stack has no repository now."})
		return
	}
	info := fmt.Sprintf("The stack uses the repository %s now.", repo.Name)
	n, err := s.refreshSnapshots(ctx, st)
	var re *refreshError
	switch {
	case errors.As(err, &re) && re.noRepo:
		s.renderSettings(w, r, st, settingsView{Info: info,
			Error: "There is no restic repository at this location yet (initialize it on the Repositories tab): " + re.msg})
	case err != nil:
		s.renderSettings(w, r, st, settingsView{Info: info, Error: "restic could not open the repository: " + err.Error()})
	default:
		s.renderSettings(w, r, st, settingsView{Info: fmt.Sprintf("%s restic found %s of this stack.", info, plural(n, "snapshot"))})
	}
}
