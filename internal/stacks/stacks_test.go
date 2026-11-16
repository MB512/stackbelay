package stacks

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func mkStack(t *testing.T, root, name, composeName string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if composeName != "" {
		if err := os.WriteFile(filepath.Join(dir, composeName), []byte("services: {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDiscover(t *testing.T) {
	root := t.TempDir()
	mkStack(t, root, "nextcloud", "compose.yml")
	mkStack(t, root, "app", "compose.yaml")
	mkStack(t, root, "nocompose", "")
	mkStack(t, root, "legacy", "docker-compose.yml")
	mkStack(t, root, "legacy2", "docker-compose.yaml")
	mkStack(t, root, "other", "stack.yml") // not a compose file name
	mkStack(t, root, ".hidden", "compose.yml")
	// Leftovers of a restore must not show up as stacks.
	mkStack(t, root, "app.old-20261113T202615Z", "compose.yml")
	mkStack(t, root, "app.old-keep", "compose.yml")
	mkStack(t, root, "app.restore-tmp", "compose.yaml")
	if err := os.WriteFile(filepath.Join(root, "file.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// Symlinked stack directory must be ignored.
	if err := os.Symlink(filepath.Join(root, "app"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}

	got, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"app", "legacy", "legacy2", "nextcloud"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want names %v", got, want)
	}
	for i, s := range got {
		if s.Name != want[i] {
			t.Errorf("stack %d: got %q, want %q", i, s.Name, want[i])
		}
		if s.Dir != filepath.Join(root, s.Name) {
			t.Errorf("stack %q: wrong dir %q", s.Name, s.Dir)
		}
	}
	if got[0].ComposeFile != filepath.Join(root, "app", "compose.yaml") {
		t.Errorf("unexpected compose file %q", got[0].ComposeFile)
	}
}

func TestDiscoverMainFilePreference(t *testing.T) {
	tests := []struct {
		name  string
		files []string
		want  string
	}{
		{"compose.yaml first", []string{"docker-compose.yaml", "docker-compose.yml", "compose.yml", "compose.yaml"}, "compose.yaml"},
		{"compose.yml before docker-compose.*", []string{"docker-compose.yaml", "docker-compose.yml", "compose.yml"}, "compose.yml"},
		{"docker-compose.yml before .yaml", []string{"docker-compose.yaml", "docker-compose.yml"}, "docker-compose.yml"},
		{"docker-compose.yaml only", []string{"docker-compose.yaml"}, "docker-compose.yaml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			for _, f := range tt.files {
				mkStack(t, root, "web", f)
			}
			got, err := Discover(root)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 {
				t.Fatalf("got %v, want one stack", got)
			}
			if want := filepath.Join(root, "web", tt.want); got[0].ComposeFile != want {
				t.Errorf("ComposeFile = %q, want %q", got[0].ComposeFile, want)
			}
		})
	}
}

func TestMainFileNamesReturnsCopy(t *testing.T) {
	MainFileNames()[0] = "changed"
	if MainFileNames()[0] != "compose.yaml" {
		t.Fatal("MainFileNames exposes its internal slice")
	}
}

func TestDiscoverRelativeRoot(t *testing.T) {
	if _, err := Discover("relative/path"); err == nil {
		t.Fatal("expected error for relative root")
	}
}

func TestLookup(t *testing.T) {
	root := t.TempDir()
	mkStack(t, root, "web", "compose.yml")

	tests := []struct {
		name    string
		wantErr bool
	}{
		{"web", false},
		{"missing", true},
		{"../web", true},
		{"web/../web", true},
		{"..", true},
		{".", true},
		{"", true},
		{"/etc", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := Lookup(root, tt.name)
			if tt.wantErr {
				if !errors.Is(err, ErrNotFound) {
					t.Fatalf("want ErrNotFound, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if s.Name != tt.name {
				t.Fatalf("got %q", s.Name)
			}
		})
	}
}

func TestValidName(t *testing.T) {
	for name, valid := range map[string]bool{
		"nextcloud":                true,
		"example.com":              true,
		"my_stack-2":               true,
		"web.old-20261113T202615Z": false,
		"web.old-keep":             false,
		"web.restore-tmp":          false,
		"web.restore-tmp2":         true,
		"bold-move":                true,
		"-web":                     false,
		"web/sub":                  false,
		strings.Repeat("a", 129):   false,
	} {
		if got := ValidName(name); got != valid {
			t.Errorf("ValidName(%q) = %v, want %v", name, got, valid)
		}
	}
}

func TestRestoreDirsAreNotStacks(t *testing.T) {
	s := Stack{Name: "web", Dir: "/stacks/web"}
	for _, dir := range []string{StagingDir(s), OldDir(s, time.Now())} {
		if ValidName(filepath.Base(dir)) {
			t.Errorf("%s would be discovered as a stack", dir)
		}
		if filepath.Dir(dir) != "/stacks" {
			t.Errorf("%s is not a sibling of the stack directory", dir)
		}
	}
	if got := OldDir(s, time.Date(2026, 11, 13, 21, 26, 15, 0, time.FixedZone("CET", 3600))); got != "/stacks/web.old-20261113T202615Z" {
		t.Errorf("OldDir = %s", got)
	}
}
