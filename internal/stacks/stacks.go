// Package stacks discovers Docker Compose stacks under STACKS_ROOT.
//
// Discovery is the only source of stack names and paths: every direct
// subdirectory of the root that contains a compose file (see MainFileNames)
// is a stack, and the directory name is the stack name.
package stacks

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// mainFileNames are the file names that mark a directory as a stack, in the
// order "docker compose" looks for them: the first one found is the main file.
var mainFileNames = []string{"compose.yaml", "compose.yml", "docker-compose.yml", "docker-compose.yaml"}

// MainFileNames returns the accepted compose main file names in order of
// preference. The result is a copy and may be modified by the caller.
func MainFileNames() []string {
	return append([]string(nil), mainFileNames...)
}

// namePattern restricts stack names to safe directory names. It excludes
// path separators, "." and ".." and leading dots/dashes.
var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// Sibling directories created by the restore pipeline next to a stack
// directory. They contain a compose file but are not stacks: names matching
// them are reserved and never discovered.
const (
	stagingSuffix = ".restore-tmp"
	oldInfix      = ".old-"
)

// reservedPattern matches staging directories and previous versions kept
// after a restore (also when renamed by hand, e.g. "web.old-keep").
var reservedPattern = regexp.MustCompile(`(\.restore-tmp$|\.old-)`)

// StagingDir is the directory a restore writes into before the swap.
func StagingDir(s Stack) string { return s.Dir + stagingSuffix }

// OldDir is where the previous stack directory is moved during a restore.
func OldDir(s Stack, t time.Time) string {
	return s.Dir + oldInfix + t.UTC().Format("20060102T150405Z")
}

// ErrNotFound is returned by Lookup when no stack with that name was discovered.
var ErrNotFound = errors.New("stack not found")

// Stack is a discovered compose stack.
type Stack struct {
	Name        string // directory name, e.g. "nextcloud"
	Dir         string // absolute path, e.g. "/home/docker/nextcloud"
	ComposeFile string // absolute path of the main compose file, e.g. compose.yaml
}

// ValidName reports whether name is an acceptable stack name. Names reserved
// for restore staging/previous directories are not.
func ValidName(name string) bool {
	return namePattern.MatchString(name) && name != "." && name != ".." &&
		!reservedPattern.MatchString(name)
}

// Discover lists all stacks directly below root, sorted by name.
// root must be an absolute path.
func Discover(root string) ([]Stack, error) {
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("stacks root %q is not an absolute path", root)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("reading stacks root: %w", err)
	}
	var out []Stack
	for _, e := range entries {
		// Symlinked directories are deliberately ignored: a symlink could
		// point outside STACKS_ROOT.
		if !e.IsDir() || !ValidName(e.Name()) {
			continue
		}
		dir := filepath.Join(root, e.Name())
		cf, err := findComposeFile(dir)
		if err != nil {
			return nil, err
		}
		if cf == "" {
			continue
		}
		out = append(out, Stack{Name: e.Name(), Dir: dir, ComposeFile: cf})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Lookup returns the discovered stack with the given name. Any name that is
// not in the discovered set (including names with path components) yields
// ErrNotFound.
func Lookup(root, name string) (Stack, error) {
	if !ValidName(name) {
		return Stack{}, fmt.Errorf("invalid stack name %q: %w", name, ErrNotFound)
	}
	all, err := Discover(root)
	if err != nil {
		return Stack{}, err
	}
	for _, s := range all {
		if s.Name == name {
			return s, nil
		}
	}
	return Stack{}, fmt.Errorf("stack %q: %w", name, ErrNotFound)
}

// HasComposeFile reports whether dir directly contains a compose file
// (regular file, not a symlink).
func HasComposeFile(dir string) (bool, error) {
	cf, err := findComposeFile(dir)
	return cf != "", err
}

func findComposeFile(dir string) (string, error) {
	for _, name := range mainFileNames {
		p := filepath.Join(dir, name)
		fi, err := os.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("checking %s: %w", p, err)
		}
		if fi.Mode().IsRegular() {
			return p, nil
		}
	}
	return "", nil
}
