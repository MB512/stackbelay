package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ErrInvalidCompose marks a compose file that "docker compose config"
// rejects; the error message is compose's own explanation.
var ErrInvalidCompose = errors.New("invalid compose file")

// ValidateCompose checks content as the main compose file of the stack in
// dir without changing the stack: it is written to a temporary file
// outside the stack directory and checked with "docker compose
// --project-directory <dir> --file <tmp> config --quiet", so relative
// paths and the stack's .env resolve as for the real file. name is the
// file name used in error messages instead of the temporary path. A
// rejected file returns an error wrapping ErrInvalidCompose.
func (c *CLI) ValidateCompose(ctx context.Context, dir, name string, content []byte) error {
	tmp, err := os.CreateTemp("", "stackvault-compose-*"+filepath.Ext(name))
	if err != nil {
		return fmt.Errorf("validating compose file: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("validating compose file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("validating compose file: %w", err)
	}

	cmd := exec.CommandContext(ctx, c.binary(), "compose", "--project-directory", dir, "--file", tmp.Name(), "config", "--quiet")
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err = cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &exitErr) && ctx.Err() == nil:
		msg := strings.TrimSpace(strings.ReplaceAll(stderr.String(), tmp.Name(), name))
		if msg == "" {
			msg = exitErr.Error()
		}
		return fmt.Errorf("%w: %s", ErrInvalidCompose, tail(msg))
	default:
		return fmt.Errorf("validating compose file: %w", err)
	}
}
