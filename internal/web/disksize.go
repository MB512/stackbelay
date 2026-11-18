package web

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"path/filepath"
	"sync"
	"time"

	"github.com/MB512/stackvault/internal/auth"
	"github.com/MB512/stackvault/internal/store"
)

// Stack size on disk (issue #9): on request ("Calculate" on the Backups
// tab), the manager walks the stack directory read-only and sums the sizes
// of its files, e.g. to estimate the first backup. It follows no symlinks;
// the result is kept in memory until the manager restarts.

const (
	diskSizeTimeout  = 5 * time.Minute
	diskSizeCooldown = 30 * time.Second
	maxDiskSizeRuns  = 2 // calculations at the same time, all stacks
)

// diskSize is the result of one calculation.
type diskSize struct {
	Bytes      uint64
	Files      int
	Unreadable int    // entries that could not be read (skipped)
	FirstError string // the first of them, for the page
	At         time.Time
	Took       time.Duration
	Err        string // the calculation itself failed (e.g. timeout)
}

// diskSizes holds the last result per stack and the running calculations.
// The zero value is ready to use; it is safe for concurrent use.
type diskSizes struct {
	mu      sync.Mutex
	results map[int64]diskSize
	running map[int64]bool
}

func (d *diskSizes) get(stackID int64) *diskSize {
	d.mu.Lock()
	defer d.mu.Unlock()
	if r, ok := d.results[stackID]; ok {
		return &r
	}
	return nil
}

// start reserves a calculation of the stack; "" if it may run, else why
// not.
func (d *diskSizes) start(stackID int64, now time.Time) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.running == nil {
		d.running, d.results = map[int64]bool{}, map[int64]diskSize{}
	}
	switch {
	case d.running[stackID]:
		return "The size of this stack is being calculated already."
	case len(d.running) >= maxDiskSizeRuns:
		return "Other stack sizes are being calculated; try again in a moment."
	}
	if r, ok := d.results[stackID]; ok && now.Sub(r.At) < diskSizeCooldown {
		return "The size was calculated just now."
	}
	d.running[stackID] = true
	return ""
}

func (d *diskSizes) finish(stackID int64, r diskSize) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.running, stackID)
	d.results[stackID] = r
}

// measureDir sums the sizes of the regular files below dir (apparent size,
// what a backup reads). Symlinks are not followed; unreadable entries are
// counted and skipped.
func measureDir(ctx context.Context, dir string) (diskSize, error) {
	var r diskSize
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error { //nolint:gosec // G703: dir is a stack directory from discovery (stackDir), not from the request
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			if p == dir {
				return err // the stack directory itself
			}
			r.Unreadable++
			if r.FirstError == "" {
				r.FirstError = err.Error()
			}
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil // directories are walked, symlinks and devices not counted
		}
		info, err := d.Info()
		if err != nil {
			r.Unreadable++
			return nil
		}
		r.Bytes += uint64(max(info.Size(), 0))
		r.Files++
		return nil
	})
	return r, err
}

// calculateStackSize measures the stack directory (view right) and shows
// the result with the snapshot list.
func (s *Server) calculateStackSize(w http.ResponseWriter, r *http.Request) {
	st, perm, ok := s.loadStack(w, r, auth.ActionView)
	if !ok {
		return
	}
	var info, errMsg string
	dir, found := s.stackDir(st)
	switch {
	case !found:
		errMsg = "The stack directory is missing."
	default:
		if reason := s.diskSizes.start(st.ID, s.now()); reason != "" {
			info = reason
			break
		}
		s.audit(r, store.AuditEntry{UserID: authFrom(r.Context()).User.ID, Action: "stack.size", StackID: st.ID})
		// Without the request's cancellation: a proxy may give up on a long
		// request, but the result is still kept for the next page view.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), diskSizeTimeout)
		started := time.Now()
		res, err := measureDir(ctx, dir)
		cancel()
		res.At, res.Took = s.now(), time.Since(started)
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			res.Err = fmt.Sprintf("not finished within %s", formatDuration(diskSizeTimeout))
		case err != nil:
			res.Err = truncate(err.Error(), 300)
		}
		if res.Err != "" {
			s.Logger.Warn("calculating stack size", "stack", st.Name, "error", res.Err)
		}
		s.diskSizes.finish(st.ID, res)
	}
	if !isHTMX(r) {
		http.Redirect(w, r, "/stacks/"+url.PathEscape(st.Name)+"/"+tabBackups, http.StatusSeeOther)
		return
	}
	page, err := s.loadBackupsTab(r, st, perm)
	if err != nil {
		s.internalError(w, r, "loading stack", err)
		return
	}
	page.SizeInfo, page.SizeError = info, errMsg
	s.renderPartial(w, "stack.html", "stack-size", page)
}
