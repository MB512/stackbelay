package stacks

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
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
	mkStack(t, root, "legacy", "docker-compose.yml") // not supported
	mkStack(t, root, ".hidden", "compose.yml")
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
	want := []string{"app", "nextcloud"}
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
