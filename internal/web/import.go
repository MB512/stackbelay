package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MB512/stackbelay/internal/auth"
	"github.com/MB512/stackbelay/internal/restic"
	"github.com/MB512/stackbelay/internal/stacks"
	"github.com/MB512/stackbelay/internal/store"
)

// Import stack (issue #1, admin): create the record of a stack that is not
// on this host yet, from a repository holding its snapshots from another
// host. The name is never typed in: it comes from the snapshot paths of the
// repository (last path component, validated like a discovered name).
//
// Step 1 lists the snapshots (read-only restic) of a configured repository
// or of a new one. The credentials of a new one wait on the server for
// step 2 (pendingImports: in memory, short lived, bound to the admin), so
// they never travel through the page; it is only saved when the import is
// confirmed.

// RepoSnapshotLister lists the snapshots of a repository that is not saved
// yet (*restic.CLI, read-only).
type RepoSnapshotLister interface {
	Snapshots(ctx context.Context, repo restic.Repo, ids ...string) ([]restic.Snapshot, error)
}

const (
	pendingImportTTL = 15 * time.Minute
	maxPendingImport = 20
)

// importCandidate is one stack name and host found in a repository.
type importCandidate struct {
	Name      string
	Host      string
	Latest    time.Time
	Snapshots int
	// Taken explains why the name cannot be imported ("" if it can): a
	// stack of that name exists in the database or on disk.
	Taken string
	// Existing: a stack record of that name exists; the page links to its
	// Settings tab (restore it there, or forget it, issue #38).
	Existing bool
}

// pendingImport is step 1's result, kept on the server until step 2.
type pendingImport struct {
	userID int64
	// repoID is the configured repository; 0 = the new repository in
	// newRepo, created on confirmation.
	repoID     int64
	newRepo    repoInput
	newForm    repoForm // newRepo's form, to show again (no secrets)
	candidates []importCandidate
	expires    time.Time
}

// pendingImports holds pending imports by random token. The zero value is
// ready to use; it is safe for concurrent use.
type pendingImports struct {
	mu sync.Mutex
	m  map[string]pendingImport
}

func (p *pendingImports) put(pi pendingImport, now time.Time) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m == nil {
		p.m = map[string]pendingImport{}
	}
	maps.DeleteFunc(p.m, func(_ string, v pendingImport) bool { return now.After(v.expires) })
	if len(p.m) >= maxPendingImport {
		return "", errors.New("too many imports in progress; try again in a few minutes")
	}
	p.m[token] = pi
	return token, nil
}

// take returns the pending import of token for userID and removes it.
func (p *pendingImports) take(token string, userID int64, now time.Time) (pendingImport, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pi, ok := p.m[token]
	if !ok || pi.userID != userID || now.After(pi.expires) {
		return pendingImport{}, false
	}
	delete(p.m, token)
	return pi, true
}

// importPage is the data of the admin "Import" tab.
type importPage struct {
	Repos []store.RepositoryInfo // configured repositories to choose from
	New   repoForm               // step 1 input after an error (never a password)
	// RepoID is the chosen configured repository (step 1 input, step 2).
	RepoID     int64
	Token      string // step 2
	RepoName   string // step 2: name of the chosen or new repository
	Location   string // step 2: its location
	Candidates []importCandidate
	ThisHost   string
	Count      int // snapshots in the repository
}

// adminImport shows step 1: the repository form.
func (s *Server) adminImport(w http.ResponseWriter, r *http.Request) {
	s.renderImport(w, r, http.StatusOK, importPage{}, "")
}

func (s *Server) renderImport(w http.ResponseWriter, r *http.Request, status int, page importPage, errMsg string) {
	repos, err := s.Store.ListRepositories(r.Context())
	if err != nil {
		s.internalError(w, r, "listing repositories", err)
		return
	}
	page.Repos, page.ThisHost = repos, s.Hostname
	page.New.prepare()
	s.render(w, r, status, "admin_import.html", pageData{Title: "Import stack", Error: errMsg, Data: page})
}

// listImport is step 1: read the snapshots of a configured repository
// (form field repository) or of a new one (name, location, password, env)
// and offer the stacks found in them.
func (s *Server) listImport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	fail := func(status int, msg string, page importPage) {
		s.renderImport(w, r, status, page, msg)
	}
	if s.RepoSnapshots == nil {
		fail(http.StatusServiceUnavailable, "Listing repositories is not available in this installation.", importPage{})
		return
	}
	pi := pendingImport{userID: authFrom(ctx).User.ID, expires: s.now().Add(pendingImportTTL)}
	var page importPage
	var repo restic.Repo
	if v := r.PostFormValue("repository"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		var stored store.Repository
		if err == nil {
			stored, err = s.Store.Repository(ctx, id)
		}
		if errors.Is(err, store.ErrNotFound) || id <= 0 {
			fail(http.StatusBadRequest, "Choose a repository from the list.", importPage{})
			return
		}
		if err != nil {
			s.internalError(w, r, "loading repository", err)
			return
		}
		pi.repoID, repo = stored.ID, stored.Restic()
		page = importPage{RepoID: stored.ID, RepoName: stored.Name, Location: stored.Location}
	} else {
		in, echo, msg := parseRepoInput(r, nil)
		if msg != "" {
			fail(http.StatusBadRequest, msg, importPage{New: echo})
			return
		}
		if taken, err := s.repoNameTaken(ctx, in.name); err != nil {
			s.internalError(w, r, "checking repository name", err)
			return
		} else if taken {
			fail(http.StatusConflict, "A repository with this name exists already. Choose it above, or enter another name.", importPage{New: echo})
			return
		}
		pi.newRepo, pi.newForm = in, echo
		repo = store.Repository{Location: in.location, Password: in.password, Env: in.env}.Restic()
		page = importPage{New: echo, RepoName: in.name, Location: in.location}
	}

	lctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	snaps, err := s.RepoSnapshots.Snapshots(lctx, repo)
	cancel()
	if err != nil {
		s.Logger.Warn("listing snapshots for an import", "error", err)
		msg := err.Error()
		var re *restic.Error
		if errors.As(err, &re) {
			msg = re.Message()
		}
		fail(http.StatusBadRequest, "restic could not list the repository: "+truncate(msg, 300), importPage{New: page.New, RepoID: page.RepoID})
		return
	}
	candidates, err := s.importCandidates(ctx, snaps)
	if err != nil {
		s.internalError(w, r, "checking stack names", err)
		return
	}
	page.Candidates, page.Count = candidates, len(snaps)
	if len(candidates) > 0 {
		pi.candidates = candidates
		token, err := s.imports.put(pi, s.now())
		if err != nil {
			fail(http.StatusServiceUnavailable, capitalize(err.Error())+".", importPage{New: page.New, RepoID: page.RepoID})
			return
		}
		page.Token = token
	}
	s.renderImport(w, r, http.StatusOK, page, "")
}

// repoNameTaken reports whether a repository has that name.
func (s *Server) repoNameTaken(ctx context.Context, name string) (bool, error) {
	repos, err := s.Store.ListRepositories(ctx)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(repos, func(ri store.RepositoryInfo) bool { return ri.Name == name }), nil
}

// importCandidates groups the snapshots by stack name (last component of
// their single path) and host. Snapshots with several paths or a name that
// discovery would not accept are left out.
func (s *Server) importCandidates(ctx context.Context, snaps []restic.Snapshot) ([]importCandidate, error) {
	type key struct{ name, host string }
	found := map[key]*importCandidate{}
	for _, sn := range snaps {
		if len(sn.Paths) != 1 || !strings.HasPrefix(sn.Paths[0], "/") {
			continue
		}
		name := path.Base(path.Clean(sn.Paths[0]))
		if !stacks.ValidName(name) || strings.ContainsAny(sn.Hostname, "\x00\n") {
			continue
		}
		k := key{name, sn.Hostname}
		c := found[k]
		if c == nil {
			c = &importCandidate{Name: name, Host: sn.Hostname}
			found[k] = c
		}
		c.Snapshots++
		if sn.Time.After(c.Latest) {
			c.Latest = sn.Time
		}
	}
	out := make([]importCandidate, 0, len(found))
	for _, c := range found {
		out = append(out, *c)
	}
	slices.SortFunc(out, func(a, b importCandidate) int {
		if n := strings.Compare(a.Name, b.Name); n != 0 {
			return n
		}
		return strings.Compare(a.Host, b.Host)
	})
	for i := range out {
		taken, existing, err := s.nameTaken(ctx, out[i].Name)
		if err != nil {
			return nil, err
		}
		out[i].Taken, out[i].Existing = taken, existing
	}
	return out, nil
}

// nameTaken explains why a stack of that name cannot be imported, or "".
// existing reports a stack record of that name. A record without
// directory and without its old setup (forgotten, see store.StackReusable)
// does not block the name: the import reuses it.
func (s *Server) nameTaken(ctx context.Context, name string) (msg string, existing bool, err error) {
	st, err := s.Store.StackByName(ctx, name)
	switch {
	case err == nil:
		reusable, err := s.Store.StackReusable(ctx, st.ID)
		if err != nil {
			return "", true, err
		}
		if !reusable {
			return "a stack of this name exists: restore it from its Backups tab, or forget it on its Settings tab", true, nil
		}
		existing = true
	case !errors.Is(err, store.ErrNotFound):
		return "", false, err
	}
	if s.StacksRoot == "" {
		return "the stacks root is not configured", existing, nil
	}
	if _, err := stacks.ImportTarget(s.StacksRoot, name); err != nil {
		return "a directory of this name exists", existing, nil
	}
	return "", existing, nil
}

// pathExists reports whether anything exists at the stack's path under the
// stacks root (a directory, also one of an incomplete import, a file or a
// symlink). Without a stacks root it reports true (nothing is known).
func (s *Server) pathExists(name string) bool {
	if s.StacksRoot == "" {
		return true
	}
	_, err := os.Lstat(filepath.Join(s.StacksRoot, name))
	return !errors.Is(err, fs.ErrNotExist)
}

// forgetStack resets the record of a stack whose directory is gone (admin
// only, issue #38): permissions, schedules, repository choice and source
// host go, so an import of the name starts clean.
func (s *Server) forgetStack(w http.ResponseWriter, r *http.Request) {
	st, _, ok := s.loadStack(w, r, auth.ActionView)
	if !ok {
		return
	}
	ctx := r.Context()
	if st.Present || s.pathExists(st.Name) {
		s.renderSettings(w, r, st, settingsView{Status: http.StatusConflict,
			Error: "The stack directory exists: only a stack whose directory is gone can be forgotten."})
		return
	}
	err := s.Store.ForgetStack(ctx, st.ID)
	if errors.Is(err, store.ErrStackBusy) {
		s.renderSettings(w, r, st, settingsView{Status: http.StatusConflict,
			Error: "The stack has a queued or running job, or its directory exists. Try again when it has finished."})
		return
	}
	if err != nil {
		s.internalError(w, r, "forgetting stack", err)
		return
	}
	s.audit(r, store.AuditEntry{UserID: authFrom(ctx).User.ID, Action: "stack.forget", StackID: st.ID,
		Detail: "permissions, schedules, repository choice and source host removed"})
	st.SourceHost = ""
	s.renderSettings(w, r, st, settingsView{Info: "The stack's setup is forgotten. Import stack can import this name again."})
}

// confirmImport is step 2: save the new repository (if step 1 entered
// one), create the stack record with the repository and fill its snapshot
// cache.
func (s *Server) confirmImport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a := authFrom(ctx)
	pi, ok := s.imports.take(r.PostFormValue("token"), a.User.ID, s.now())
	if !ok {
		s.renderImport(w, r, http.StatusBadRequest, importPage{}, "This import has expired. Choose the repository again.")
		return
	}
	again := importPage{RepoID: pi.repoID, New: pi.newForm}
	i, err := strconv.Atoi(r.PostFormValue("choice"))
	if err != nil || i < 0 || i >= len(pi.candidates) {
		s.renderImport(w, r, http.StatusBadRequest, again, "Choose a stack to import.")
		return
	}
	c := pi.candidates[i]
	// Checked again: something may have changed since step 1.
	if taken, _, err := s.nameTaken(ctx, c.Name); err != nil {
		s.internalError(w, r, "checking stack name", err)
		return
	} else if taken != "" {
		s.renderImport(w, r, http.StatusConflict, again, fmt.Sprintf("%s cannot be imported: %s.", c.Name, taken))
		return
	}

	repoID, repoName := pi.repoID, ""
	if repoID == 0 {
		in := pi.newRepo
		repoID, err = s.Store.CreateRepository(ctx, in.name, in.location, in.password, in.env)
		if errors.Is(err, store.ErrConflict) {
			s.renderImport(w, r, http.StatusConflict, again, "A repository with this name exists already. Choose it, or enter another name.")
			return
		}
		if err != nil {
			s.internalError(w, r, "saving repository", err)
			return
		}
		repoName = in.name
		s.audit(r, store.AuditEntry{UserID: a.User.ID, Action: "repository.create",
			Detail: fmt.Sprintf("repository %q (id %d), location %q, variables %v (import)", truncate(in.name, 100), repoID,
				truncate(in.location, 200), slices.Sorted(maps.Keys(in.env)))})
	} else {
		ri, err := s.Store.RepositoryInfo(ctx, repoID)
		if errors.Is(err, store.ErrNotFound) {
			s.renderImport(w, r, http.StatusConflict, importPage{}, "The repository was deleted in the meantime.")
			return
		}
		if err != nil {
			s.internalError(w, r, "loading repository", err)
			return
		}
		repoName = ri.Name
	}

	source := c.Host
	if source == s.Hostname {
		source = "" // this host
	}
	st, err := s.Store.CreateImportedStack(ctx, c.Name, source)
	if errors.Is(err, store.ErrConflict) {
		s.renderImport(w, r, http.StatusConflict, importPage{}, c.Name+" exists already.")
		return
	}
	if err != nil {
		s.internalError(w, r, "creating stack", err)
		return
	}
	if err := s.Store.SetStackRepository(ctx, st.ID, repoID); err != nil {
		s.internalError(w, r, "setting repository", err)
		return
	}
	s.audit(r, store.AuditEntry{UserID: a.User.ID, Action: "stack.import", StackID: st.ID,
		Detail: fmt.Sprintf("stack %s from host %q, repository %q (id %d)", c.Name, c.Host, truncate(repoName, 100), repoID)})
	if _, err := s.refreshSnapshots(ctx, st); err != nil {
		s.Logger.Warn("refreshing snapshots of an imported stack", "stack", st.Name, "error", err)
	}
	http.Redirect(w, r, "/stacks/"+st.Name+"/backups", http.StatusSeeOther) //nolint:gosec // G710: name validated (stacks.ValidName)
}
