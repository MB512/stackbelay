package jobs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/MB512/stackvault/internal/docker"
	"github.com/MB512/stackvault/internal/restic"
)

var errBoom = errors.New("boom")

// calls is a shared, ordered record of all fake calls, so tests can assert
// the sequence across docker and restic.
type calls struct {
	mu  sync.Mutex
	log []string
}

func (c *calls) add(format string, args ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.log = append(c.log, fmt.Sprintf(format, args...))
}

func (c *calls) list() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.log...)
}

type fakeDocker struct {
	calls    *calls
	services []docker.ServiceState
	failOn   map[string]error // method name -> error
	// startCtxErr records ctx.Err() seen by Start/Up, to verify that the
	// restart is not affected by job cancellation.
	startCtxErr error
	onUp        func() // called by Up, to inspect the stack directory at start
	// upFiles and upOverride record the compose files of the last Up and
	// the content of its last file (the generated override is removed
	// after the job).
	upFiles    []string
	upOverride string
	// missingImages are references ImageExists reports as absent.
	missingImages []string
	// upFailures makes the next n Up calls fail with errBoom (besides failOn).
	upFailures int
	// servicesAfter, if set, is returned by Services from the second call
	// on (the services of an edited compose file).
	servicesAfter []docker.ServiceState
	servicesCalls int
	// images is returned by ConfiguredImages.
	images map[string]docker.ConfiguredImage
}

func (f *fakeDocker) ConfiguredImages(ctx context.Context, dir string, services []string) (map[string]docker.ConfiguredImage, error) {
	f.calls.add("docker.ConfiguredImages %s", strings.Join(services, ","))
	return f.images, f.fail("ConfiguredImages")
}

func (f *fakeDocker) fail(m string) error { return f.failOn[m] }

func (f *fakeDocker) Services(ctx context.Context, dir string) ([]docker.ServiceState, error) {
	f.calls.add("docker.Services")
	f.servicesCalls++
	if err := f.failOn["ServicesAfter"]; err != nil && f.servicesCalls > 1 {
		return nil, err
	}
	if f.servicesAfter != nil && f.servicesCalls > 1 {
		return f.servicesAfter, f.fail("Services")
	}
	return f.services, f.fail("Services")
}

func (f *fakeDocker) Pull(ctx context.Context, dir string, services []string) error {
	f.calls.add("docker.Pull %s", strings.Join(services, ","))
	return f.fail("Pull")
}

func (f *fakeDocker) Stop(ctx context.Context, dir string) error {
	f.calls.add("docker.Stop")
	return f.fail("Stop")
}

func (f *fakeDocker) Start(ctx context.Context, dir string, services []string) error {
	f.calls.add("docker.Start %s", strings.Join(services, ","))
	f.startCtxErr = ctx.Err()
	return f.fail("Start")
}

func (f *fakeDocker) Up(ctx context.Context, dir string, composeFiles, services []string) error {
	f.upFiles = composeFiles
	if len(composeFiles) > 0 {
		b, _ := os.ReadFile(composeFiles[len(composeFiles)-1])
		f.upOverride = string(b)
	}
	if services == nil {
		f.calls.add("docker.Up <all>")
	} else {
		f.calls.add("docker.Up %s", strings.Join(services, ","))
	}
	f.startCtxErr = ctx.Err()
	if f.onUp != nil {
		f.onUp()
	}
	if f.upFailures > 0 {
		f.upFailures--
		return errBoom
	}
	return f.fail("Up")
}

func (f *fakeDocker) EnsureImage(ctx context.Context, ref string) (bool, error) {
	f.calls.add("docker.EnsureImage %s", ref)
	return true, f.fail("EnsureImage")
}

func (f *fakeDocker) UpAll(ctx context.Context, dir string) error {
	f.calls.add("docker.UpAll")
	return f.fail("UpAll")
}

func (f *fakeDocker) Down(ctx context.Context, dir string) error {
	f.calls.add("docker.Down")
	return f.fail("Down")
}

func (f *fakeDocker) ImageExists(ctx context.Context, ref string) (bool, error) {
	f.calls.add("docker.ImageExists %s", ref)
	return !slices.Contains(f.missingImages, ref), f.fail("ImageExists")
}

func (f *fakeDocker) WaitHealthy(ctx context.Context, dir string, services []string) error {
	f.calls.add("docker.WaitHealthy")
	return f.fail("WaitHealthy")
}

type fakeRestic struct {
	calls     *calls
	failOn    map[string]error // "CheckRepo", "Backup:<tag>", "Restore", "Snapshots"
	snapshots []restic.Snapshot
	// files written into the restore target by Restore (name -> content).
	restoreFiles map[string]string
	// node is returned by Node (metadata of the stack directory).
	node restic.Node
	// cancel, if set, is called during the final backup to simulate a
	// cancelled job.
	cancelOnFinal context.CancelFunc
	// ids overrides the snapshot id returned by Backup per tag (default
	// "snap-<tag>").
	ids map[string]string
	// inPlaceVisit, if set, is called by RestoreInPlace with the target.
	inPlaceVisit func(target string)
}

func (f *fakeRestic) CheckRepo(ctx context.Context, repo restic.Repo) error {
	f.calls.add("restic.CheckRepo")
	return f.failOn["CheckRepo"]
}

func (f *fakeRestic) Backup(ctx context.Context, repo restic.Repo, path string, opts restic.BackupOptions) (string, error) {
	tag := strings.Join(opts.Tags, ",")
	f.calls.add("restic.Backup tag=%s parent=%s host=%s", tag, opts.Parent, opts.Host)
	if tag == TagFinal && f.cancelOnFinal != nil {
		f.cancelOnFinal()
		return "", context.Canceled
	}
	if err := f.failOn["Backup:"+tag]; err != nil {
		return "", err
	}
	if id, ok := f.ids[tag]; ok {
		return id, nil
	}
	return "snap-" + tag, nil
}

func (f *fakeRestic) Snapshots(ctx context.Context, repo restic.Repo, ids ...string) ([]restic.Snapshot, error) {
	f.calls.add("restic.Snapshots %s", strings.Join(ids, ","))
	return f.snapshots, f.failOn["Snapshots"]
}

func (f *fakeRestic) Restore(ctx context.Context, repo restic.Repo, id, subpath, target string) error {
	f.calls.add("restic.Restore %s:%s", id, subpath)
	// Like restic, create the target (0700) and write some files before failing.
	if err := os.MkdirAll(target, 0o700); err != nil {
		return err
	}
	for name, content := range f.restoreFiles {
		if err := os.WriteFile(filepath.Join(target, name), []byte(content), 0o644); err != nil {
			return err
		}
	}
	return f.failOn["Restore"]
}

// RestoreInPlace writes restoreFiles into target and, like restic
// --delete, removes other top-level entries that are not excluded.
func (f *fakeRestic) RestoreInPlace(ctx context.Context, repo restic.Repo, id, subpath, target string, exclude []string) error {
	f.calls.add("restic.RestoreInPlace %s:%s exclude=%s", id, subpath, strings.Join(exclude, ","))
	if f.inPlaceVisit != nil {
		f.inPlaceVisit(target)
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if _, ok := f.restoreFiles[e.Name()]; ok || slices.ContainsFunc(exclude, func(x string) bool {
			return strings.SplitN(x, "/", 2)[0] == e.Name()
		}) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(target, e.Name())); err != nil {
			return err
		}
	}
	for name, content := range f.restoreFiles {
		if err := os.WriteFile(filepath.Join(target, name), []byte(content), 0o644); err != nil {
			return err
		}
	}
	return f.failOn["RestoreInPlace"]
}

func (f *fakeRestic) Node(ctx context.Context, repo restic.Repo, id, path string) (restic.Node, error) {
	f.calls.add("restic.Node %s:%s", id, path)
	n := f.node
	n.Path = path
	return n, f.failOn["Node"]
}
