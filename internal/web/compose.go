package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/MB512/stackbelay/internal/auth"
	"github.com/MB512/stackbelay/internal/docker"
	"github.com/MB512/stackbelay/internal/jobs"
	"github.com/MB512/stackbelay/internal/stacks"
	"github.com/MB512/stackbelay/internal/store"
)

// ComposeValidator checks an edited compose file without changing the
// stack (*docker.CLI: "docker compose config --quiet" on a temporary copy).
type ComposeValidator interface {
	ValidateCompose(ctx context.Context, dir, name string, content []byte) error
}

// composeValidateTimeout bounds the validation of an edited compose file.
const composeValidateTimeout = 30 * time.Second

// composeTab is the data of the admin-only "Compose" tab.
type composeTab struct {
	stackHeader
	CSRF       string
	FileName   string // base name of the main compose file
	Content    string // the editor's text
	BaseSHA256 string // SHA-256 of the file the editor started from
	HasRepo    bool
	// Unavailable says why the file cannot be edited here; the editor is
	// then not shown.
	Unavailable string
	// JobsUnavailable says why nothing can be applied right now.
	JobsUnavailable string
	// Problem is a validation error or conflict to show above the editor.
	Problem string
}

// loadComposeTab reads the stack's main compose file for the editor. The
// file comes from discovery (stacks.Lookup), never from the request.
func (s *Server) loadComposeTab(r *http.Request, st store.Stack) (composeTab, stacks.Stack, []byte, error) {
	a := authFrom(r.Context())
	page := composeTab{
		stackHeader:     s.newStackHeader(a.User, store.Permission{}, st, tabCompose), // admin-only tab
		CSRF:            a.Session.CSRFToken,
		JobsUnavailable: s.jobsUnavailable(),
	}
	_, err := s.Store.RepositoryInfoForStack(r.Context(), st.ID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return page, stacks.Stack{}, nil, err
	}
	page.HasRepo = err == nil

	found, err := stacks.Lookup(s.StacksRoot, st.Name)
	if s.StacksRoot == "" || err != nil {
		page.Unavailable = "The stack directory is missing."
		return page, stacks.Stack{}, nil, nil
	}
	page.FileName = filepath.Base(found.ComposeFile)
	current, err := readComposeFile(found.ComposeFile)
	switch {
	case errors.Is(err, errComposeTooLarge):
		page.Unavailable = fmt.Sprintf("%s is larger than %d KiB and cannot be edited here.", page.FileName, jobs.MaxComposeBytes>>10)
	case err != nil:
		return page, found, nil, err
	case !utf8.Valid(current) || bytes.IndexByte(current, 0) >= 0:
		page.Unavailable = page.FileName + " is not a text file in UTF-8 and cannot be edited here."
	default:
		page.Content = string(current)
		page.BaseSHA256 = jobs.SHA256Hex(current)
	}
	return page, found, current, nil
}

var errComposeTooLarge = errors.New("compose file too large")

// readComposeFile reads at most jobs.MaxComposeBytes of a compose file.
func readComposeFile(path string) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the main compose file from stack discovery
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, jobs.MaxComposeBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > jobs.MaxComposeBytes {
		return nil, errComposeTooLarge
	}
	return b, nil
}

// stackCompose shows the compose editor (admin only).
func (s *Server) stackCompose(w http.ResponseWriter, r *http.Request) {
	st, _, ok := s.loadStack(w, r, auth.ActionView) // the route requires an admin
	if !ok {
		return
	}
	page, _, _, err := s.loadComposeTab(r, st)
	if err != nil {
		s.internalError(w, r, "reading compose file", err)
		return
	}
	s.renderCompose(w, r, http.StatusOK, page)
}

func (s *Server) renderCompose(w http.ResponseWriter, r *http.Request, status int, page composeTab) {
	s.render(w, r, status, "stack_compose.html", pageData{Title: page.Stack.Name + " compose file", Data: page})
}

// applyCompose validates the edited compose file and queues a
// compose-apply job (admin only). The editor's text is kept on every
// error, so nothing typed is lost.
func (s *Server) applyCompose(w http.ResponseWriter, r *http.Request) {
	st, _, ok := s.loadStack(w, r, auth.ActionView) // the route requires an admin
	if !ok {
		return
	}
	page, found, current, err := s.loadComposeTab(r, st)
	if err != nil {
		s.internalError(w, r, "reading compose file", err)
		return
	}
	if page.Unavailable != "" {
		s.renderCompose(w, r, http.StatusConflict, page)
		return
	}
	// Browsers send textarea line breaks as CRLF.
	content := strings.ReplaceAll(r.PostFormValue("content"), "\r\n", "\n")
	base := r.PostFormValue("base_sha256")
	page.Content = content
	retry := func(status int, problem string) {
		page.Problem = problem
		page.BaseSHA256 = base // a conflict stays a conflict until the page is reloaded
		s.renderCompose(w, r, status, page)
	}
	switch {
	case base != jobs.SHA256Hex(current):
		retry(http.StatusConflict, page.FileName+" was changed after you opened it. Your text is kept below; "+
			"copy what you need and reload the page to edit the current version.")
		return
	case content == string(current):
		retry(http.StatusOK, "There are no changes.")
		return
	case strings.TrimSpace(content) == "":
		retry(http.StatusUnprocessableEntity, "The compose file must not be empty.")
		return
	case len(content) > jobs.MaxComposeBytes:
		retry(http.StatusUnprocessableEntity, fmt.Sprintf("The compose file must not be larger than %d KiB.", jobs.MaxComposeBytes>>10))
		return
	case !utf8.ValidString(content) || strings.ContainsRune(content, 0):
		retry(http.StatusUnprocessableEntity, "The compose file must be UTF-8 text.")
		return
	case !page.HasRepo && r.PostFormValue("confirm_no_backup") != "on":
		retry(http.StatusBadRequest, "Confirm that the change is applied without backup and without rollback.")
		return
	}
	if s.Compose == nil {
		retry(http.StatusServiceUnavailable, "Compose files cannot be validated in this installation.")
		return
	}
	vctx, cancel := context.WithTimeout(r.Context(), composeValidateTimeout)
	err = s.Compose.ValidateCompose(vctx, found.Dir, page.FileName, []byte(content))
	cancel()
	if err != nil {
		msg := err.Error()
		if errors.Is(err, docker.ErrInvalidCompose) {
			msg = strings.TrimPrefix(msg, docker.ErrInvalidCompose.Error()+": ")
		}
		retry(http.StatusUnprocessableEntity, "docker compose rejected the file: "+msg)
		return
	}

	sum := jobs.SHA256Hex([]byte(content))
	job := store.Job{Type: jobs.TypeComposeApply, ComposeContent: []byte(content), Options: store.JobOptions{
		ComposeFile: page.FileName, ComposeBaseSHA256: base, ComposeSHA256: sum, NoBackup: !page.HasRepo,
	}}
	s.submitJob(w, r, st, job, fmt.Sprintf("compose-apply of %s: sha256 %s, %d bytes, backup: %t",
		page.FileName, sum, len(content), page.HasRepo), page.HasRepo)
}
