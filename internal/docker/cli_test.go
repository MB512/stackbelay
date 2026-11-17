package docker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestParsePS(t *testing.T) {
	want := []psEntry{
		{ID: "abc", Service: "app", State: "running", Health: "healthy", Image: "nextcloud:29"},
		{ID: "def", Service: "db", State: "exited", Image: "postgres:16", ExitCode: 137},
	}
	tests := []struct {
		name string
		in   string
		want []psEntry
	}{
		{"empty", "", nil},
		{"empty array", "[]", []psEntry{}},
		{"array", `[{"ID":"abc","Service":"app","State":"running","Health":"healthy","Image":"nextcloud:29"},{"ID":"def","Service":"db","State":"exited","Health":"","Image":"postgres:16","ExitCode":137}]`, want},
		{"ndjson", `{"ID":"abc","Service":"app","State":"running","Health":"healthy","Image":"nextcloud:29","Extra":1}
{"ID":"def","Service":"db","State":"exited","Health":"","Image":"postgres:16","ExitCode":137}
`, want},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parsePS([]byte(tt.in))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
	if _, err := parsePS([]byte("{not json")); err == nil {
		t.Fatal("expected error for invalid json")
	}
}

func TestHealthStatus(t *testing.T) {
	cs := []psEntry{
		{Service: "ok", State: "running"},
		{Service: "healthy", State: "running", Health: "healthy"},
		{Service: "starting", State: "running", Health: "starting"},
		{Service: "created", State: "created"},
		{Service: "sick", State: "running", Health: "unhealthy"},
		{Service: "dead", State: "exited"},
		{Service: "scaled", State: "running"},
		{Service: "scaled", State: "restarting"},
	}
	tests := []struct {
		name        string
		services    []string
		wantPending []string
		wantErr     string
	}{
		{"all ready", []string{"ok", "healthy"}, nil, ""},
		{"pending", []string{"ok", "starting", "created", "missing", "scaled"}, []string{"starting", "created", "missing", "scaled"}, ""},
		{"unhealthy fails", []string{"ok", "sick"}, nil, "sick (unhealthy)"},
		{"exited fails", []string{"dead", "starting"}, nil, "dead (exited)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pending, err := healthStatus(cs, tt.services)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(pending, tt.wantPending) {
				t.Fatalf("pending = %v, want %v", pending, tt.wantPending)
			}
		})
	}
}

func TestProjectOwner(t *testing.T) {
	cfg, err := parseProjectConfig([]byte(`{"name":"stackvault","services":{"web":{},"db":{"image":"x"}}}`))
	if err != nil || cfg.Name != "stackvault" || !slices.Equal(cfg.Services, []string{"db", "web"}) {
		t.Fatalf("parseProjectConfig: %+v %v", cfg, err)
	}
	if _, err := parseProjectConfig([]byte(`{"services":{}}`)); err == nil {
		t.Fatal("missing name accepted")
	}

	const dir = "/home/docker/stackvault"
	tests := []struct {
		name        string
		out         string
		wantForeign []string
		wantLegacy  []string
	}{
		{"none", "", nil, nil},
		{"own (trailing slash, no version)", "stackvault-manager-1\t/home/docker/stackvault\t2.29.1\nstackvault-db-1\t/home/docker/stackvault/\n", nil, nil},
		{"other directory", "stackvault-manager-1\t/tmp/test/stacks/stackvault\t2.29.1\n",
			[]string{"stackvault-manager-1 (/tmp/test/stacks/stackvault)"}, nil},
		{"started without compose labels", "stackvault-x\t\t\n", []string{"stackvault-x ()"}, nil},
		{"docker-compose v1", "stackvault_web_1\t\t1.25.0\nstackvault_db_1\t\t1.25.0\n", nil,
			[]string{"stackvault_web_1", "stackvault_db_1"}},
		{"docker-compose v1 from this directory is fine", "stackvault_web_1\t/home/docker/stackvault\t1.29.2\n", nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			foreign, legacy := classifyContainers([]byte(tt.out), dir)
			if !slices.Equal(foreign, tt.wantForeign) || !slices.Equal(legacy, tt.wantLegacy) {
				t.Errorf("foreign %q, legacy %q; want %q, %q", foreign, legacy, tt.wantForeign, tt.wantLegacy)
			}
		})
	}

	for in, want := range map[string]string{"stackvault": "stackvault", "my-app": "myapp", "My_App2": "myapp2"} {
		if got := legacyProjectName(in); got != want {
			t.Errorf("legacyProjectName(%q) = %q, want %q", in, got, want)
		}
	}
}

// overviewDocker writes a docker script for StackOverview of the project
// "my-app": v2 holds the "docker ps" lines of the project itself, v1 those
// of the old docker-compose v1 name "myapp".
func overviewDocker(t *testing.T, v2, v1 string) string {
	t.Helper()
	script := `#!/bin/sh
case "$*" in
  *" config --format json") echo '{"name":"my-app","services":{"web":{},"db":{}}}' ;;
  *"label=com.docker.compose.project=my-app "*) printf '` + v2 + `' ;;
  *"label=com.docker.compose.project=myapp "*) printf '` + v1 + `' ;;
  *" ps --all --format json") echo '{"ID":"0000000000a1","Name":"my-app-web-1","Project":"my-app","Service":"web","State":"exited","ExitCode":1}' ;;
  *) echo "unexpected: $*" >&2; exit 1 ;;
esac
`
	bin := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil { //nolint:gosec // G306: test script must be executable
		t.Fatal(err)
	}
	return bin
}

func TestStackOverview(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "my-app")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	c := &CLI{Binary: overviewDocker(t, "my-app-web-1\\t"+dir+"\\t2.29.1\\n", `myapp_web_1\t/elsewhere\t2.29.1\n`)}
	ov, err := c.StackOverview(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	want := Overview{Services: []string{"db", "web"}, Containers: []Container{
		{ID: "0000000000a1", Name: "web-1", Service: "web", State: "exited", ExitCode: 1}}}
	if !reflect.DeepEqual(ov, want) {
		t.Fatalf("overview %+v, want %+v", ov, want)
	}

	// docker-compose v1 containers under the old project name.
	c = &CLI{Binary: overviewDocker(t, ``, `myapp_web_1\t\t1.17.1\n`)}
	if _, err := c.StackOverview(ctx, dir); !errors.Is(err, ErrLegacyCompose) || !strings.Contains(err.Error(), "myapp_web_1") {
		t.Fatalf("v1 containers: %v", err)
	}
	// Containers of another directory with the same name.
	c = &CLI{Binary: overviewDocker(t, `my-app-web-1\t/other/my-app\t2.29.1\n`, ``)}
	if _, err := c.StackOverview(ctx, dir); err == nil || errors.Is(err, ErrLegacyCompose) ||
		!strings.Contains(err.Error(), "another directory") {
		t.Fatalf("foreign containers: %v", err)
	}
}
