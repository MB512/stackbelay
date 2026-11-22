package web

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/MB512/stackbelay/internal/auth"
	"github.com/MB512/stackbelay/internal/restic"
	"github.com/MB512/stackbelay/internal/store"
)

// SnapshotRefresher refreshes the snapshot cache of a stack by asking
// restic (read-only). *snapshots.Cache implements it; tests use a fake.
type SnapshotRefresher interface {
	Refresh(ctx context.Context, stackID int64) ([]store.CachedSnapshot, error)
}

// refreshTimeout bounds a snapshot refresh started from the UI.
const refreshTimeout = 60 * time.Second

// repoForm is the repository section of the Settings tab. It never holds
// secret values, only the names of stored variables.
type repoForm struct {
	Exists   bool
	Location string   // current location, or the submitted one after an error
	EnvKeys  []string // names of the stored backend variables
	Allowed  []string // variable names that may be entered
	// OfferInit shows the "Initialize repository" button.
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

// saveRepository creates or updates the repository of a stack (admin
// only), then checks it by refreshing the snapshot cache.
//
// Form fields: location (a REST URL may contain user:password, which is
// moved into the encrypted variables), password (empty = keep), env
// ("KEY=value" lines; non-empty replaces all stored variables), clear_env.
func (s *Server) saveRepository(w http.ResponseWriter, r *http.Request) {
	st, _, ok := s.loadStack(w, r, auth.ActionView)
	if !ok {
		return
	}
	ctx := r.Context()
	old, err := s.Store.RepositoryForStack(ctx, st.ID)
	exists := err == nil
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.internalError(w, r, "loading repository", err)
		return
	}
	fail := func(msg, location string) {
		s.renderSettings(w, r, st, settingsView{Status: http.StatusBadRequest, Error: msg, Location: location})
	}

	location, urlEnv, err := restic.SplitRESTCredentials(strings.TrimSpace(r.PostFormValue("location")))
	if err != nil {
		fail(capitalize(err.Error())+".", "") // the input may contain a password: do not echo it
		return
	}
	password := r.PostFormValue("password")
	if password == "" && exists {
		password = old.Password
	}
	env := map[string]string{}
	if exists && r.PostFormValue("clear_env") != "on" {
		maps.Copy(env, old.Env)
	}
	if text := r.PostFormValue("env"); strings.TrimSpace(text) != "" {
		if env, err = parseEnvLines(text); err != nil {
			fail(capitalize(err.Error())+".", location)
			return
		}
	}
	maps.Copy(env, urlEnv) // credentials from the URL win
	if err := store.ValidateRepository(location, password, env); err != nil {
		fail(capitalize(err.Error())+".", location)
		return
	}

	if _, err := s.Store.SetRepository(ctx, st.ID, location, password, env); err != nil {
		s.internalError(w, r, "saving repository", err)
		return
	}
	keys := slices.Sorted(maps.Keys(env))
	s.audit(r, store.AuditEntry{UserID: authFrom(ctx).User.ID, Action: "repository.update", StackID: st.ID,
		Detail: fmt.Sprintf("location %q, variables %v, password changed: %t",
			truncate(location, 200), keys, !exists || password != old.Password)})

	info := "Repository saved."
	if len(urlEnv) > 0 {
		info += " The user name and password from the URL are stored encrypted as RESTIC_REST_USERNAME/RESTIC_REST_PASSWORD."
	}
	s.renderSettings(w, r, st, s.checkRepository(ctx, st, info))
}

// checkRepository refreshes the snapshot cache as an access check and
// describes the result for the Settings tab. If there is no repository at
// the location, it offers to initialize one.
func (s *Server) checkRepository(ctx context.Context, st store.Stack, info string) settingsView {
	n, err := s.refreshSnapshots(ctx, st)
	var re *refreshError
	switch {
	case errors.As(err, &re) && re.noRepo:
		return settingsView{Info: info, OfferInit: true,
			Error: "There is no restic repository at this location yet: " + re.msg}
	case err != nil:
		return settingsView{Info: info, Error: "restic could not open the repository: " + err.Error()}
	}
	return settingsView{Info: fmt.Sprintf("%s restic found %s.", info, plural(n, "snapshot"))}
}

// refreshError is a refresh failure prepared for display.
type refreshError struct {
	msg    string // shortened restic message, safe to show
	noRepo bool   // restic: no repository at the location
}

func (e *refreshError) Error() string { return e.msg }

// refreshSnapshots runs a snapshot cache refresh and returns the number of
// snapshots of this stack and host that exist in the repository. Errors are *refreshError, with a
// shortened message for display; restic output never contains the
// credentials (see restic.CLI).
func (s *Server) refreshSnapshots(ctx context.Context, st store.Stack) (int, error) {
	if s.Snapshots == nil {
		return 0, &refreshError{msg: "snapshot refresh is not available"}
	}
	ctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
	_, err := s.Snapshots.Refresh(ctx, st.ID)
	if err != nil {
		s.Logger.Warn("refreshing snapshots", "stack", st.Name, "error", err)
		msg := err.Error()
		// errors.As finds a *restic.Error anywhere in the wrapped chain,
		// like catching a specific exception type (or InnerException) in C#.
		var re *restic.Error
		if errors.As(err, &re) {
			msg = re.Message()
		} else if errors.Is(err, context.DeadlineExceeded) {
			msg = "no answer within " + refreshTimeout.String()
		}
		return 0, &refreshError{msg: truncate(msg, 300), noRepo: restic.IsNoRepository(err)}
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

// loadRepoForm returns the repository section data for the Settings tab.
func (s *Server) loadRepoForm(ctx context.Context, st store.Stack) (repoForm, error) {
	f := repoForm{Allowed: allowedRepoEnv()}
	repo, err := s.Store.RepositoryForStack(ctx, st.ID)
	if errors.Is(err, store.ErrNotFound) {
		return f, nil
	}
	if err != nil {
		return f, err
	}
	f.Exists, f.Location = true, repo.Location
	f.EnvKeys = slices.Sorted(maps.Keys(repo.Env))
	return f, nil
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

// initRepository runs "restic init" at the stack's stored location (admin
// only), then checks the new repository. The button is only offered after
// restic reported that no repository exists there, and restic itself
// refuses to init over an existing repository.
func (s *Server) initRepository(w http.ResponseWriter, r *http.Request) {
	st, _, ok := s.loadStack(w, r, auth.ActionView)
	if !ok {
		return
	}
	ctx := r.Context()
	repo, err := s.Store.RepositoryForStack(ctx, st.ID)
	if errors.Is(err, store.ErrNotFound) {
		s.renderError(w, r, http.StatusBadRequest, "This stack has no repository.")
		return
	}
	if err != nil {
		s.internalError(w, r, "loading repository", err)
		return
	}
	if s.Repos == nil {
		s.renderSettings(w, r, st, settingsView{Status: http.StatusServiceUnavailable, Error: "Initializing repositories is not available."})
		return
	}

	s.audit(r, store.AuditEntry{UserID: authFrom(ctx).User.ID, Action: "repository.init", StackID: st.ID,
		Detail: fmt.Sprintf("location %q", truncate(repo.Location, 200))})
	initCtx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
	if err := s.Repos.Init(initCtx, repo.Restic()); err != nil {
		s.Logger.Warn("initializing repository", "stack", st.Name, "error", err)
		msg := err.Error()
		var re *restic.Error
		if errors.As(err, &re) {
			msg = re.Message()
		}
		s.renderSettings(w, r, st, settingsView{Error: "restic could not initialize the repository: " + truncate(msg, 300)})
		return
	}
	s.renderSettings(w, r, st, s.checkRepository(ctx, st, "Repository initialized."))
}
