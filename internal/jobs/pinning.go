package jobs

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/MB512/stackbelay/internal/stacks"
)

// Compose override file names in the order "docker compose" looks for them.
// The main file names come from stacks.MainFileNames, so pinning picks the
// same main file as stack discovery.
var (
	composeOverrideFiles = []string{"compose.override.yml", "compose.override.yaml",
		"docker-compose.override.yml", "docker-compose.override.yaml"}
)

// composeFiles returns the compose files that "docker compose" would use in
// srcDir (main file and optional override), as paths under finalDir (the
// directory the stack will have when it starts). Passing them explicitly
// with --file disables compose's own lookup, so the override file must be
// listed too. A COMPOSE_FILE setting in .env is not supported: the files
// compose would use are then unknown.
func composeFiles(srcDir, finalDir string) ([]string, error) {
	if set, err := envSetsComposeFile(filepath.Join(srcDir, ".env")); err != nil {
		return nil, err
	} else if set {
		return nil, errors.New("the stack's .env sets COMPOSE_FILE; image pinning supports only the default compose file names")
	}
	var files []string
	for _, group := range [][]string{stacks.MainFileNames(), composeOverrideFiles} {
		for _, name := range group {
			info, err := os.Stat(filepath.Join(srcDir, name))
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("checking %s: %w", name, err)
			}
			if info.Mode().IsRegular() {
				files = append(files, filepath.Join(finalDir, name))
				break // compose uses the first file found of each group
			}
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no compose file (%s) in the restored stack", strings.Join(stacks.MainFileNames(), ", "))
	}
	return files, nil
}

// envSetsComposeFile reports whether an .env file assigns COMPOSE_FILE.
func envSetsComposeFile(path string) (bool, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the .env of the restored stack directory
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading .env: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		if name, _, ok := strings.Cut(line, "="); ok && strings.TrimSpace(name) == "COMPOSE_FILE" {
			return true, nil
		}
	}
	return false, sc.Err()
}

// pinnedOverride is the generated compose override. JSON is valid YAML, so
// encoding/json writes it without hand-made quoting.
type pinnedOverride struct {
	Services map[string]pinnedService `json:"services"`
}

type pinnedService struct {
	Image      string `json:"image"`
	PullPolicy string `json:"pull_policy"`
}

// preparePinning reads metadata.txt from the restored staging directory,
// makes the recorded images available (pulling them by digest if needed)
// and writes an override file that runs exactly these images. It returns
// the compose files to start the stack with and a function that removes
// the override file. Any error aborts the restore before the swap.
func (p *Pipeline) preparePinning(ctx context.Context, log *slog.Logger, stackName, staging, finalDir string) (files []string, cleanup func(), err error) {
	f, err := os.Open(filepath.Join(staging, MetadataFile)) //nolint:gosec // G304: inside the staging directory
	if err != nil {
		return nil, nil, fmt.Errorf("image pinning: the snapshot has no %s: %w", MetadataFile, err)
	}
	// metadata.txt comes from the snapshot: ParseMetadata validates every
	// field, repo digests against RepoDigestPattern.
	m, err := ParseMetadata(f, stackName)
	_ = f.Close()
	if err != nil {
		return nil, nil, fmt.Errorf("image pinning: %w", err)
	}
	files, err = composeFiles(staging, finalDir)
	if err != nil {
		return nil, nil, fmt.Errorf("image pinning: %w", err)
	}

	override := pinnedOverride{Services: map[string]pinnedService{}}
	for _, s := range m.Services {
		if s.RepoDigest == "" {
			log.Warn("image pinning: no repo digest recorded (built locally or never created); using compose.yml",
				"service", s.Name)
			continue
		}
		p.phase("getting image " + s.RepoDigest)
		pulled, err := p.Docker.EnsureImage(ctx, s.RepoDigest)
		if err != nil {
			return nil, nil, fmt.Errorf("image pinning: service %s: getting %s: %w", s.Name, s.RepoDigest, err)
		}
		log.Info("image pinned", "service", s.Name, "image", s.RepoDigest, "pulled", pulled)
		override.Services[s.Name] = pinnedService{Image: s.RepoDigest, PullPolicy: "never"}
	}
	if len(override.Services) == 0 {
		log.Warn("image pinning: no service has a recorded repo digest; starting with compose.yml only")
		return files, func() {}, nil
	}

	data, err := json.MarshalIndent(override, "", "  ")
	if err != nil {
		return nil, nil, fmt.Errorf("image pinning: encoding override: %w", err)
	}
	tmp, err := os.CreateTemp("", "compose.pinned-*.yml")
	if err != nil {
		return nil, nil, fmt.Errorf("image pinning: %w", err)
	}
	cleanup = func() { _ = os.Remove(tmp.Name()) }
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		cleanup()
		return nil, nil, fmt.Errorf("image pinning: writing override: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("image pinning: writing override: %w", err)
	}
	return append(files, tmp.Name()), cleanup, nil
}
