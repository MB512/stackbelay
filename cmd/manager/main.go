// Command manager runs the StackVault web UI, the scheduler and job
// creation. It never runs backups itself; it starts worker containers.
//
// Configuration is read from the environment (see internal/config).
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/MB512/stackvault/internal/auth"
	"github.com/MB512/stackvault/internal/config"
	"github.com/MB512/stackvault/internal/dispatch"
	"github.com/MB512/stackvault/internal/docker"
	"github.com/MB512/stackvault/internal/restic"
	"github.com/MB512/stackvault/internal/secrets"
	"github.com/MB512/stackvault/internal/snapshots"
	"github.com/MB512/stackvault/internal/stacks"
	"github.com/MB512/stackvault/internal/store"
	"github.com/MB512/stackvault/internal/web"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	code := run(ctx, os.Getenv, os.ReadFile, os.Stderr, nil)
	stop()
	os.Exit(code)
}

// shutdownTimeout bounds how long in-flight requests may take after
// SIGTERM. Docker's default stop timeout is 10s.
const shutdownTimeout = 8 * time.Second

// run starts the manager and blocks until ctx is cancelled or the server
// fails. ready, if not nil, is called with the listen address once the
// server accepts connections (used by tests).
func run(ctx context.Context, getenv func(string) string, readFile func(string) ([]byte, error),
	stderr io.Writer, ready func(addr string)) int {
	cfg, err := config.LoadManager(getenv, readFile)
	if err != nil {
		fmt.Fprintf(stderr, "configuration error:\n%v\n", err)
		return 2
	}
	log := slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))

	st, err := openStore(ctx, cfg)
	if err != nil {
		log.Error("starting manager", "error", err)
		return 1
	}
	defer st.Close()

	if err := auth.EnsureInitialAdmin(ctx, st, stderr, log); err != nil {
		log.Error("starting manager: initial admin", "error", err)
		return 1
	}
	if err := syncStacks(ctx, cfg.StacksRoot, st, log); err != nil {
		log.Error("starting manager", "error", err)
		return 1
	}

	resticCLI := &restic.CLI{Logger: log}
	snapCache := &snapshots.Cache{Store: st, Restic: resticCLI}
	disp, jobsErr := newDispatcher(ctx, cfg, st, snapCache, log)
	if jobsErr != nil {
		// The UI still works (stacks, repositories, users); jobs are
		// shown as unavailable with this reason.
		log.Error("backups and restores are unavailable", "error", jobsErr)
	}
	srv := &web.Server{
		Store: st, Logger: log, SessionTTL: cfg.SessionTTL, SecureCookies: !cfg.AllowInsecureHTTP,
		// The manager runs only read-only restic commands itself, plus
		// "restic init" on an explicit admin action.
		Snapshots: snapCache,
		Repos:     resticCLI,
		JobsDir:   cfg.JobsDir(),
		Hostname:  cfg.Hostname,
		// On SIGTERM open live streams end at once (clients reconnect)
		// instead of holding up the graceful shutdown.
		Stopping: ctx.Done(),
	}
	if disp != nil {
		srv.Jobs = disp
	} else {
		srv.JobsDisabledReason = "Backups and restores are unavailable: " + jobsErr.Error()
	}
	handler, err := srv.Handler()
	if err != nil {
		log.Error("starting manager", "error", err)
		return 1
	}
	httpSrv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// No WriteTimeout: live log streams (SSE) stay open for long.
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.ListenAddr)
	if err != nil {
		log.Error("starting manager", "error", err)
		return 1
	}
	log.Info("manager listening", "addr", ln.Addr().String(), "stacks_root", cfg.StacksRoot, "data_dir", cfg.DataDir)
	if cfg.AllowInsecureHTTP {
		log.Warn("STACKVAULT_ALLOW_INSECURE_HTTP is set: logins over plain HTTP are accepted; use a TLS reverse proxy in production")
	}

	// Serve in a goroutine (a lightweight thread, like Task.Run in C#) and
	// report its result through a channel, so the main goroutine can wait
	// for either the server failing or a shutdown signal (like
	// Task.WhenAny).
	serveErr := make(chan error, 1)
	go func() { serveErr <- httpSrv.Serve(ln) }()
	if ready != nil {
		ready(ln.Addr().String())
	}

	go cleanupSessions(ctx, st, log)
	if disp != nil {
		go disp.Run(ctx)
	}

	select {
	case err := <-serveErr:
		log.Error("http server failed", "error", err)
		return 1
	case <-ctx.Done():
	}

	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Warn("graceful shutdown incomplete", "error", err)
	}
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		log.Error("http server failed", "error", err)
		return 1
	}
	return 0
}

// openStore creates the data directories and opens the database.
func openStore(ctx context.Context, cfg config.Manager) (*store.Store, error) {
	if err := os.MkdirAll(cfg.JobsDir(), 0o700); err != nil {
		return nil, fmt.Errorf("creating data directories: %w", err)
	}
	box, err := secrets.New(cfg.SecretKey)
	if err != nil {
		return nil, err
	}
	st, err := store.Open(ctx, cfg.DBPath(), box)
	if err != nil {
		return nil, fmt.Errorf("opening database %s: %w", cfg.DBPath(), err)
	}
	return st, nil
}

// syncStacks mirrors the discovered stacks into the database.
func syncStacks(ctx context.Context, root string, st *store.Store, log *slog.Logger) error {
	found, err := stacks.Discover(root)
	if err != nil {
		return fmt.Errorf("discovering stacks: %w", err)
	}
	names := make([]string, 0, len(found))
	for _, s := range found {
		names = append(names, s.Name)
	}
	if err := st.SyncStacks(ctx, names); err != nil {
		return err
	}
	log.Info("stacks discovered", "count", len(names), "stacks", names)
	return nil
}

// cleanupSessions deletes expired sessions once an hour until ctx is done.
func cleanupSessions(ctx context.Context, st *store.Store, log *slog.Logger) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := st.DeleteExpiredSessions(ctx, time.Now())
			if err != nil && ctx.Err() == nil {
				log.Warn("deleting expired sessions", "error", err)
			} else if n > 0 {
				log.Info("deleted expired sessions", "count", n)
			}
		}
	}
}

// newDispatcher builds the job dispatcher. It inspects the manager's own
// container (if it runs in one) to find the host side of its mounts.
func newDispatcher(ctx context.Context, cfg config.Manager, st *store.Store, snaps dispatch.SnapshotRefresher,
	log *slog.Logger) (*dispatch.Dispatcher, error) {
	cli := &docker.CLI{}
	var self *docker.SelfInfo
	if mountinfo, err := os.ReadFile("/proc/self/mountinfo"); err == nil {
		if id := docker.OwnContainerID(mountinfo); id != "" {
			ictx, cancel := context.WithTimeout(ctx, 10*time.Second)
			info, err := cli.InspectSelf(ictx, id)
			cancel()
			if err != nil {
				return nil, fmt.Errorf("inspecting the manager container (is the Docker socket mounted?): %w", err)
			}
			self = &info
		}
	}
	tpl, err := dispatch.NewTemplate(dispatch.TemplateConfig{
		WorkerImage: cfg.WorkerImage, StacksRoot: cfg.StacksRoot, JobsDir: cfg.JobsDir(),
		Hostname: cfg.Hostname, LogLevel: cfg.LogLevel.String(),
	}, self)
	if err != nil {
		return nil, err
	}
	log.Info("worker template", "image", tpl.Image, "jobs_mount", tpl.Jobs.Type+":"+tpl.Jobs.Source+"/"+tpl.Jobs.SubPath,
		"docker_socket", tpl.Socket, "max_jobs", cfg.MaxJobs)
	return &dispatch.Dispatcher{
		Store: st, Docker: cli, Template: tpl, JobsDir: cfg.JobsDir(), Snapshots: snaps,
		Logger: log, MaxRunning: cfg.MaxJobs,
	}, nil
}
