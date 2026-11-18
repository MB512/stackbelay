package docker

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const (
	imgA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	imgB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	imgC = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	ctr1 = "1111111111111111111111111111111111111111111111111111111111111111"
	ctr2 = "2222222222222222222222222222222222222222222222222222222222222222"
	ctr3 = "3333333333333333333333333333333333333333333333333333333333333333"
)

// cleanupDocker writes a docker script for the cleanup commands: dangling
// images A, B and C, two containers (one uses B), and the compose project
// "my-app" (services web, db) with an orphan "old" (stopped) and "cron"
// (running). Every call is appended to the returned args file.
func cleanupDocker(t *testing.T, dir string) (bin, argsFile string) {
	t.Helper()
	tmp := t.TempDir()
	argsFile = filepath.Join(tmp, "args")
	script := `#!/bin/sh
echo "$*" >> ` + argsFile + `
case "$*" in
  "image ls --filter dangling=true --no-trunc --format {{.ID}}") printf '` + imgA + `\n` + imgB + `\n` + imgC + `\n' ;;
  "ps --all --quiet --no-trunc") printf '` + ctr1 + `\n` + ctr2 + `\n' ;;
  "container inspect --format {{.Image}} -- ` + ctr1 + ` ` + ctr2 + `") printf 'sha256:dddd\n` + imgB + `\n' ;;
  "image inspect -- ` + imgA + ` ` + imgC + `") cat <<'EOF'
[{"Id":"` + imgA + `","Created":"2025-01-02T03:04:05.123456789Z","Size":1000,"RepoDigests":["app@sha256:1111"],
  "Metadata":{"LastTagTime":"2026-11-01T10:00:00Z"}},
 {"Id":"` + imgC + `","Created":"2026-10-01T00:00:00Z","Size":20,"RepoDigests":[],
  "Metadata":{"LastTagTime":"0001-01-01T00:00:00Z"}}]
EOF
  ;;
  *" config --format json") echo '{"name":"my-app","services":{"web":{},"db":{}}}' ;;
  "ps --all --filter label=com.docker.compose.project=my-app "*) printf 'my-app-web-1\t` + dir + `\t2.29.1\n' ;;
  "ps --all --filter label=com.docker.compose.project=myapp "*) ;;
  "ps --all --no-trunc --filter label=com.docker.compose.project=my-app "*)
    printf '` + ctr1 + `\tmy-app-web-1\trunning\tweb\n` + ctr2 + `\tmy-app-old-1\texited\told\n` + ctr3 + `\tmy-app-cron-1\trunning\tcron\n' ;;
  "image rm -- "*|"container rm -- "*) ;;
  *) echo "unexpected: $*" >&2; exit 1 ;;
esac
`
	bin = filepath.Join(tmp, "docker")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil { //nolint:gosec // G306: test script must be executable
		t.Fatal(err)
	}
	return bin, argsFile
}

func TestDanglingImages(t *testing.T) {
	bin, _ := cleanupDocker(t, "/stacks/my-app")
	c := &CLI{Binary: bin}
	got, err := c.DanglingImages(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// B is used by a container and left out.
	want := []DanglingImage{
		{ID: imgA, Created: time.Date(2025, 1, 2, 3, 4, 5, 123456789, time.UTC),
			LastTagTime: time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC), Size: 1000, RepoDigests: []string{"app@sha256:1111"}},
		{ID: imgC, Created: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), Size: 20, RepoDigests: []string{}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestStackOrphans(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "my-app")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	bin, _ := cleanupDocker(t, dir)
	c := &CLI{Binary: bin}
	got, err := c.StackOrphans(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []OrphanContainer{
		{ID: ctr2, Name: "my-app-old-1", Service: "old", State: "exited"},
		{ID: ctr3, Name: "my-app-cron-1", Service: "cron", State: "running"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	if got[0].Running() || !got[1].Running() {
		t.Fatal("Running wrong")
	}
}

func TestParseOrphansRejectsOddOutput(t *testing.T) {
	for _, in := range []string{"short\tname\texited\told\n", ctr1 + "\tname\texited\n"} {
		if _, err := parseOrphans([]byte(in), nil); err == nil {
			t.Errorf("accepted %q", in)
		}
	}
}

func TestCleanupRemovals(t *testing.T) {
	bin, argsFile := cleanupDocker(t, "/stacks/my-app")
	c := &CLI{Binary: bin}
	ctx := context.Background()
	if err := c.RemoveImage(ctx, imgA); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveStoppedContainer(ctx, ctr2); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"", "-f", "sha256:abc", imgA + " --force", "nextcloud:29"} {
		if c.RemoveImage(ctx, bad) == nil {
			t.Errorf("RemoveImage(%q) accepted", bad)
		}
		if c.RemoveStoppedContainer(ctx, bad) == nil {
			t.Errorf("RemoveStoppedContainer(%q) accepted", bad)
		}
	}
	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if want := "image rm -- " + imgA + "\ncontainer rm -- " + ctr2 + "\n"; string(args) != want {
		t.Fatalf("docker calls:\n%s\nwant (no --force, nothing else):\n%s", args, want)
	}
	if strings.Contains(string(args), "prune") {
		t.Fatal("prune used")
	}
}
