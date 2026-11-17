package jobs

import (
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/MB512/stackvault/internal/restic"
)

// ProgressFile is the name of the progress file in a job directory. The
// worker rewrites it while the job runs; the manager shows it on the job
// page. It is informational only: nothing depends on it.
const ProgressFile = "progress.json"

const maxProgressBytes = 64 << 10

// Progress is what a running job is doing right now.
type Progress struct {
	Version int    `json:"version"`
	Phase   string `json:"phase"`            // e.g. "final snapshot", "rollback: restoring"
	Detail  string `json:"detail,omitempty"` // e.g. "waiting for: app, db"
	// Transfer is set while restic backs up or restores.
	Transfer  *Transfer `json:"transfer,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Transfer is the progress of a restic backup or restore (see
// restic.Progress).
type Transfer struct {
	Percent          float64 `json:"percent"` // 0..1
	BytesDone        uint64  `json:"bytes_done"`
	TotalBytes       uint64  `json:"total_bytes"`
	FilesDone        uint64  `json:"files_done"`
	TotalFiles       uint64  `json:"total_files"`
	SecondsElapsed   int64   `json:"seconds_elapsed,omitempty"`
	SecondsRemaining int64   `json:"seconds_remaining,omitempty"`
}

// BytesPerSecond is the average speed so far, 0 if unknown.
func (t Transfer) BytesPerSecond() uint64 {
	if t.SecondsElapsed <= 0 {
		return 0
	}
	return t.BytesDone / uint64(t.SecondsElapsed)
}

// ProgressReporter receives the progress of a pipeline. Implementations
// must be safe for concurrent use (Detail may come from a goroutine that
// watches containers).
type ProgressReporter interface {
	// Phase starts a new phase; detail and transfer are cleared.
	Phase(name string)
	// Detail describes the current phase further ("" clears it).
	Detail(detail string)
	// Transfer reports restic's progress in the current phase.
	Transfer(p restic.Progress)
}

// phase reports a new phase (with the rollback prefix, if any).
func (p *Pipeline) phase(name string) {
	if p.Progress != nil {
		p.Progress.Phase(p.phasePrefix + name)
	}
}

// FileProgress writes progress.json into a job directory (worker side).
// Phases, details and the first transfer update of a phase are written at
// once, further transfer updates at most once per MinInterval. Write errors are ignored: progress is informational and
// must never fail a job. The zero value is not usable; set Dir.
type FileProgress struct {
	Dir string
	// MinInterval between two transfer updates; default 1 s.
	MinInterval time.Duration
	// Now defaults to time.Now (tests override it).
	Now func() time.Time

	// mu guards the fields below, like a C# lock: transfer updates come
	// from restic's output, details possibly from another goroutine.
	mu        sync.Mutex
	cur       Progress
	lastWrite time.Time
}

var _ ProgressReporter = (*FileProgress)(nil)

func (f *FileProgress) now() time.Time {
	if f.Now == nil {
		return time.Now()
	}
	return f.Now()
}

// Phase implements ProgressReporter.
func (f *FileProgress) Phase(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cur = Progress{Phase: name}
	f.write()
}

// Detail implements ProgressReporter.
func (f *FileProgress) Detail(detail string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cur.Detail == detail {
		return
	}
	f.cur.Detail = detail
	f.write()
}

// Transfer implements ProgressReporter.
func (f *FileProgress) Transfer(p restic.Progress) {
	f.mu.Lock()
	defer f.mu.Unlock()
	first := f.cur.Transfer == nil // the first of a phase is written at once
	f.cur.Transfer = &Transfer{Percent: p.Percent, BytesDone: p.BytesDone, TotalBytes: p.TotalBytes,
		FilesDone: p.FilesDone, TotalFiles: p.TotalFiles,
		SecondsElapsed: p.SecondsElapsed, SecondsRemaining: p.SecondsRemaining}
	interval := f.MinInterval
	if interval <= 0 {
		interval = time.Second
	}
	if !first && f.now().Sub(f.lastWrite) < interval {
		return
	}
	f.write()
}

// write stores the current progress; f.mu must be held.
func (f *FileProgress) write() {
	f.cur.Version = handoffVersion
	f.lastWrite = f.now()
	f.cur.UpdatedAt = f.lastWrite.UTC()
	_ = writeJSONAtomic(filepath.Join(f.Dir, ProgressFile), f.cur)
}

// ReadProgress reads progress.json from a job directory (manager side). It
// returns an error wrapping fs.ErrNotExist if there is none (yet).
func ReadProgress(dir string) (Progress, error) {
	var p Progress
	if err := readJSON(filepath.Join(dir, ProgressFile), maxProgressBytes, false, &p); err != nil {
		return Progress{}, fmt.Errorf("reading job progress: %w", err)
	}
	if p.Version != handoffVersion {
		return Progress{}, fmt.Errorf("unsupported progress version %d", p.Version)
	}
	return p, nil
}
