package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"syscall"

	"github.com/MB512/stackbelay/internal/docker"
	"github.com/MB512/stackbelay/internal/stacks"
)

// MaxComposeBytes is the largest compose file the editor accepts.
const MaxComposeBytes = 256 << 10

// ComposeApplyOptions control a compose-apply job (compose editor).
type ComposeApplyOptions struct {
	// Content is the new main compose file.
	Content []byte
	// BaseSHA256 is the SHA-256 (hex) of the file the editor started from.
	// If the file changed since, the job fails before anything happens.
	BaseSHA256 string
	// Backup and CarryOver: see UpdateOptions.
	Backup    bool
	CarryOver string
	// PreviousCopy, if set, is where the current content is saved before it
	// is replaced (the job directory), for a manual revert.
	PreviousCopy string
}

// SHA256Hex returns the SHA-256 of b as lowercase hex.
func SHA256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ComposeApply replaces the stack's main compose file with opts.Content
// and brings the stack up with it, like an update (see runChange): back
// up, write the file, start, verify, roll back if it does not come up.
//
// Started are the services that ran before and are still defined, plus
// services the new file adds or that have no container yet; waited for are those that ran before. A
// stack that was stopped only gets the new file. Containers of services
// the new file removes are left stopped (orphans).
func (p *Pipeline) ComposeApply(ctx context.Context, stack stacks.Stack, opts ComposeApplyOptions) (ChangeResult, error) {
	log := p.logger().With("stack", stack.Name, "job", "compose-apply")
	if len(opts.Content) == 0 || len(opts.Content) > MaxComposeBytes {
		return ChangeResult{}, fmt.Errorf("new compose file has an invalid size (%d bytes)", len(opts.Content))
	}
	return p.runChange(ctx, stack, change{
		name: "compose-apply", backup: opts.Backup, carryOver: opts.CarryOver,
		prepare: func(context.Context, []docker.ServiceState, []string) error {
			current, err := os.ReadFile(stack.ComposeFile)
			if err != nil {
				return fmt.Errorf("reading %s: %w", filepath.Base(stack.ComposeFile), err)
			}
			if SHA256Hex(current) != opts.BaseSHA256 {
				return fmt.Errorf("%s was changed after it was opened in the editor; nothing was applied, "+
					"edit the current version", filepath.Base(stack.ComposeFile))
			}
			if opts.PreviousCopy != "" {
				if err := os.WriteFile(opts.PreviousCopy, current, 0o600); err != nil { //nolint:gosec // G703: job directory + fixed name, set by the worker
					return fmt.Errorf("saving the previous compose file: %w", err)
				}
			}
			return nil
		},
		apply: func(ctx context.Context, before []docker.ServiceState, running []string) ([]string, []string, bool, error) {
			log.Info("writing the new compose file", "file", filepath.Base(stack.ComposeFile), "bytes", len(opts.Content))
			p.phase("writing the compose file")
			if err := replaceFile(stack.ComposeFile, opts.Content); err != nil {
				return nil, nil, false, fmt.Errorf("writing %s: %w", filepath.Base(stack.ComposeFile), err)
			}
			if len(running) == 0 {
				log.Info("the stack was not running; it stays stopped with the new compose file")
				return nil, nil, true, nil
			}
			after, err := p.services(ctx, log, stack.Dir)
			if err != nil {
				return nil, nil, true, fmt.Errorf("reading the services of the new compose file: %w", err)
			}
			start, verify := servicesAfterEdit(before, after, running)
			for _, s := range before {
				if !slices.ContainsFunc(after, func(a docker.ServiceState) bool { return a.Name == s.Name }) && s.ContainerID != "" {
					log.Warn("service removed from the compose file; its container is left stopped "+
						"(remove it with: docker compose up --remove-orphans)", "service", s.Name)
				}
			}
			return start, verify, true, nil
		},
	})
}

// servicesAfterEdit returns the services to start after a compose file
// change (running before and still defined, plus new ones) and those to
// verify (running before and still defined). New means: not in the file
// before, or defined but without any container yet (added while the stack
// was stopped, or by hand). A service with a stopped container stays
// stopped; that was a choice. New services are started but not waited for:
// they may be one-shot tasks.
func servicesAfterEdit(before, after []docker.ServiceState, running []string) (start, verify []string) {
	known := map[string]bool{}
	for _, s := range before {
		known[s.Name] = s.ContainerID != ""
	}
	for _, s := range after {
		switch {
		case slices.Contains(running, s.Name):
			start = append(start, s.Name)
			verify = append(verify, s.Name)
		case !known[s.Name]:
			start = append(start, s.Name)
		}
	}
	return start, verify
}

// replaceFile atomically replaces the regular file path with content,
// keeping its mode and (as root) its owner: the content goes to a
// temporary file in the same directory, which is synced and renamed over
// path.
func replaceFile(path string, content []byte) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("not a regular file")
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".stackvault-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(info.Mode().Perm()); err != nil {
		_ = tmp.Close()
		return err
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && os.Geteuid() == 0 {
		if err := tmp.Chown(int(st.Uid), int(st.Gid)); err != nil {
			_ = tmp.Close()
			return err
		}
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
