package docker

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

// fakeSlowStop is a docker stand-in whose "compose stop" takes a moment
// while "compose ps" reports app still running and db exited.
const fakeSlowStop = `#!/bin/sh
case "$*" in
  *" stop") sleep 0.4 ;;
  *" ps --format json") echo '{"Service":"app","State":"running"}'; echo '{"Service":"db","State":"exited"}' ;;
esac
`

func TestStopReportsPendingServices(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(bin, []byte(fakeSlowStop), 0o755); err != nil { //nolint:gosec // test script must be executable
		t.Fatal(err)
	}
	var mu sync.Mutex
	var reports [][]string
	c := &CLI{Binary: bin, PollInterval: 50 * time.Millisecond, Pending: func(s []string) {
		mu.Lock()
		defer mu.Unlock()
		reports = append(reports, s)
	}}
	if err := c.Stop(context.Background(), t.TempDir()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reports) < 2 {
		t.Fatalf("reports %v", reports)
	}
	if !reflect.DeepEqual(reports[0], []string{"app"}) {
		t.Errorf("first report %v, want [app]", reports[0])
	}
	if last := reports[len(reports)-1]; len(last) != 0 {
		t.Errorf("last report %v, want empty (done)", last)
	}
}

func TestStopWithoutPendingDoesNotWatch(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "docker")
	// Fails on anything but "stop": no ps without Pending.
	script := "#!/bin/sh\ncase \"$*\" in *\" stop\") exit 0 ;; *) exit 1 ;; esac\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil { //nolint:gosec // test script must be executable
		t.Fatal(err)
	}
	if err := (&CLI{Binary: bin}).Stop(context.Background(), t.TempDir()); err != nil {
		t.Fatal(err)
	}
}

// fakeHealth reports db as starting on the first ps and healthy after
// that (a counter file in the script's directory).
const fakeHealth = `#!/bin/sh
n=$(cat "$0.n" 2>/dev/null || echo 0); echo $((n+1)) > "$0.n"
case "$*" in
  *" ps --all --format json")
    echo '{"Service":"app","State":"running"}'
    if [ "$n" = 0 ]; then echo '{"Service":"db","State":"running","Health":"starting"}'
    else echo '{"Service":"db","State":"running","Health":"healthy"}'; fi ;;
esac
`

func TestWaitHealthyReportsPendingServices(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(bin, []byte(fakeHealth), 0o755); err != nil { //nolint:gosec // test script must be executable
		t.Fatal(err)
	}
	var reports [][]string
	c := &CLI{Binary: bin, PollInterval: 10 * time.Millisecond, Pending: func(s []string) { reports = append(reports, s) }}
	if err := c.WaitHealthy(context.Background(), t.TempDir(), []string{"app", "db"}); err != nil {
		t.Fatal(err)
	}
	if len(reports) != 2 || !reflect.DeepEqual(reports[0], []string{"db"}) || len(reports[1]) != 0 {
		t.Fatalf("reports %v", reports)
	}
}
