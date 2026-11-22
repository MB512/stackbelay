package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MB512/stackbelay/internal/restic"
	"github.com/MB512/stackbelay/internal/stacks"
	"github.com/MB512/stackbelay/internal/store"
)

// Import stack (issue #1, admin): create the record of a stack that is not
// on this host yet, from a repository holding its snapshots from another
// host. The name is never typed in: it comes from the snapshot paths of the
// repository (last path component, validated like a discovered name).
//
// Step 1 lists the repository's snapshots (read-only restic) and keeps the
// credentials on the server for step 2 (pendingImports: in memory, short
// lived, bound to the admin), so they never travel through the page.

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
}

// pendingImport is step 1's result, kept on the server until step 2.
type pendingImport struct {
	userID     int64
	location   string
	password   string
	env        map[string]string
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
	Location   string // step 1 input after an error (never a password)
	Allowed    []string
	Token      string // step 2
	Candidates []importCandidate
	ThisHost   string
	Count      int // snapshots in the repository
}

// adminImport shows step 1: the repository form.
func (s *Server) adminImport(w http.ResponseWriter, r *http.Request) {
	s.renderImport(w, r, http.StatusOK, importPage{}, "")
}

func (s *Server) renderImport(w http.ResponseWriter, r *http.Request, status int, page importPage, errMsg string) {
	page.Allowed, page.ThisHost = allowedRepoEnv(), s.Hostname
	s.render(w, r, status, "admin_import.html", pageData{Title: "Import stack", Error: errMsg, Data: page})
}

// listImport is step 1: read the repository's snapshots and offer the
// stacks found in them.
func (s *Server) listImport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	fail := func(status int, msg, location string) {
		s.renderImport(w, r, status, importPage{Location: location}, msg)
	}
	if s.RepoSnapshots == nil {
		fail(http.StatusServiceUnavailable, "Listing repositories is not available in this installation.", "")
		return
	}
	location, urlEnv, err := restic.SplitRESTCredentials(strings.TrimSpace(r.PostFormValue("location")))
	if err != nil {
		fail(http.StatusBadRequest, capitalize(err.Error())+".", "") // may contain a password: not echoed
		return
	}
	env := map[string]string{}
	if text := r.PostFormValue("env"); strings.TrimSpace(text) != "" {
		if env, err = parseEnvLines(text); err != nil {
			fail(http.StatusBadRequest, capitalize(err.Error())+".", location)
			return
		}
	}
	maps.Copy(env, urlEnv)
	password := r.PostFormValue("password")
	if err := store.ValidateRepository(location, password, env); err != nil {
		fail(http.StatusBadRequest, capitalize(err.Error())+".", location)
		return
	}
	repo := store.Repository{Location: location, Password: password, Env: env}.Restic()
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
		fail(http.StatusBadRequest, "restic could not list the repository: "+truncate(msg, 300), location)
		return
	}
	candidates, err := s.importCandidates(ctx, snaps)
	if err != nil {
		s.internalError(w, r, "checking stack names", err)
		return
	}
	page := importPage{Location: location, Candidates: candidates, Count: len(snaps)}
	if len(candidates) > 0 {
		token, err := s.imports.put(pendingImport{userID: authFrom(ctx).User.ID, location: location, password: password,
			env: env, candidates: candidates, expires: s.now().Add(pendingImportTTL)}, s.now())
		if err != nil {
			fail(http.StatusServiceUnavailable, capitalize(err.Error())+".", location)
			return
		}
		page.Token = token
	}
	s.renderImport(w, r, http.StatusOK, page, "")
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
		taken, err := s.nameTaken(ctx, out[i].Name)
		if err != nil {
			return nil, err
		}
		out[i].Taken = taken
	}
	return out, nil
}

// nameTaken explains why a stack of that name cannot be imported, or "".
func (s *Server) nameTaken(ctx context.Context, name string) (string, error) {
	_, err := s.Store.StackByName(ctx, name)
	switch {
	case err == nil:
		return "a stack of this name exists (restore it from its Backups tab)", nil
	case !errors.Is(err, store.ErrNotFound):
		return "", err
	}
	if s.StacksRoot == "" {
		return "the stacks root is not configured", nil
	}
	if _, err := stacks.ImportTarget(s.StacksRoot, name); err != nil {
		return "a directory of this name exists", nil
	}
	return "", nil
}

// confirmImport is step 2: create the stack record with the repository of
// step 1 and fill its snapshot cache.
func (s *Server) confirmImport(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a := authFrom(ctx)
	pi, ok := s.imports.take(r.PostFormValue("token"), a.User.ID, s.now())
	if !ok {
		s.renderImport(w, r, http.StatusBadRequest, importPage{}, "This import has expired. Enter the repository again.")
		return
	}
	i, err := strconv.Atoi(r.PostFormValue("choice"))
	if err != nil || i < 0 || i >= len(pi.candidates) {
		s.renderImport(w, r, http.StatusBadRequest, importPage{Location: pi.location}, "Choose a stack to import.")
		return
	}
	c := pi.candidates[i]
	// Checked again: something may have changed since step 1.
	if taken, err := s.nameTaken(ctx, c.Name); err != nil {
		s.internalError(w, r, "checking stack name", err)
		return
	} else if taken != "" {
		s.renderImport(w, r, http.StatusConflict, importPage{Location: pi.location}, fmt.Sprintf("%s cannot be imported: %s.", c.Name, taken))
		return
	}
	source := c.Host
	if source == s.Hostname {
		source = "" // this host
	}
	st, err := s.Store.CreateImportedStack(ctx, c.Name, source)
	if errors.Is(err, store.ErrConflict) {
		s.renderImport(w, r, http.StatusConflict, importPage{Location: pi.location}, c.Name+" exists already.")
		return
	}
	if err != nil {
		s.internalError(w, r, "creating stack", err)
		return
	}
	if _, err := s.Store.SetRepository(ctx, st.ID, pi.location, pi.password, pi.env); err != nil {
		s.internalError(w, r, "saving repository", err)
		return
	}
	s.audit(r, store.AuditEntry{UserID: a.User.ID, Action: "stack.import", StackID: st.ID,
		Detail: fmt.Sprintf("stack %s from host %q, repository %q, variables %v", c.Name, c.Host,
			truncate(pi.location, 200), slices.Sorted(maps.Keys(pi.env)))})
	if _, err := s.refreshSnapshots(ctx, st); err != nil {
		s.Logger.Warn("refreshing snapshots of an imported stack", "stack", st.Name, "error", err)
	}
	http.Redirect(w, r, "/stacks/"+st.Name+"/backups", http.StatusSeeOther) //nolint:gosec // G710: name validated (stacks.ValidName)
}
