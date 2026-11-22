package dispatch

import (
	"strings"
	"testing"
	"time"

	"github.com/MB512/stackbelay/internal/docker"
	"github.com/MB512/stackbelay/internal/restic"
)

func TestNewTemplate(t *testing.T) {
	cfg := TemplateConfig{StacksRoot: "/home/docker", JobsDir: "/data/jobs", Hostname: "host-a", JobTimeout: 72 * time.Hour,
		DowntimeTimeout: 90 * time.Minute}
	stacksMount := docker.Mount{Type: "bind", Source: "/home/docker", Target: "/home/docker"}
	socket := docker.Mount{Type: "bind", Source: "/run/user/1000/docker.sock", Target: "/var/run/docker.sock"}

	// /data as a bind mount from the stack folder.
	tpl, err := NewTemplate(cfg, &docker.SelfInfo{ImageID: "sha256:self", Mounts: []docker.Mount{
		stacksMount, socket, {Type: "bind", Source: "/home/docker/stackvault/data", Target: "/data"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if tpl.Image != "sha256:self" || tpl.Socket != "/run/user/1000/docker.sock" ||
		tpl.Jobs != (docker.Mount{Type: "bind", Source: "/home/docker/stackvault/data/jobs"}) {
		t.Fatalf("bind: %+v", tpl)
	}

	// /data as a named volume; an explicit worker image wins.
	cfg.WorkerImage = "ghcr.io/x/stackbelay:1"
	tpl, err = NewTemplate(cfg, &docker.SelfInfo{ImageID: "sha256:self", Mounts: []docker.Mount{
		stacksMount, {Type: "volume", Source: "stackvault_data", Target: "/data"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if tpl.Image != "ghcr.io/x/stackbelay:1" || tpl.Socket != "/var/run/docker.sock" ||
		tpl.Jobs != (docker.Mount{Type: "volume", Source: "stackvault_data", SubPath: "jobs"}) {
		t.Fatalf("volume: %+v", tpl)
	}
	cs, err := tpl.Spec("01ABCDEFGHJKMNPQRSTVWXYZ01", restic.Repo{Location: "/r", Env: []string{"RESTIC_PASSWORD=pw"}})
	if err != nil {
		t.Fatal(err)
	}
	if cs.Mounts[2] != (docker.Mount{Type: "volume", Source: "stackvault_data", Target: "/job", SubPath: "jobs/01ABCDEFGHJKMNPQRSTVWXYZ01"}) {
		t.Fatalf("job mount: %+v", cs.Mounts[2])
	}
	if cs.Env["STACKVAULT_JOB_TIMEOUT"] != "72h0m0s" || cs.Env["STACKVAULT_DOWNTIME_TIMEOUT"] != "1h30m0s" {
		t.Fatalf("timeout env: %q %q", cs.Env["STACKVAULT_JOB_TIMEOUT"], cs.Env["STACKVAULT_DOWNTIME_TIMEOUT"])
	}

	// Errors: stacks root not at the same path, data not mounted, no image.
	_, err = NewTemplate(cfg, &docker.SelfInfo{Mounts: []docker.Mount{
		{Type: "bind", Source: "/srv/stacks", Target: "/home/docker"}, {Type: "volume", Source: "d", Target: "/data"},
	}})
	if err == nil || !strings.Contains(err.Error(), "same path") {
		t.Fatalf("stacks root elsewhere: %v", err)
	}
	_, err = NewTemplate(cfg, &docker.SelfInfo{Mounts: []docker.Mount{stacksMount}})
	if err == nil || !strings.Contains(err.Error(), "data directory") {
		t.Fatalf("data not mounted: %v", err)
	}
	cfg.WorkerImage = ""
	if _, err := NewTemplate(cfg, nil); err == nil {
		t.Fatal("outside a container without image: want error")
	}
	cfg.WorkerImage = "img"
	tpl, err = NewTemplate(cfg, nil)
	if err != nil || tpl.Jobs != (docker.Mount{Type: "bind", Source: "/data/jobs"}) {
		t.Fatalf("outside a container: %+v %v", tpl, err)
	}
}

// Workers run in the manager's image, whose HEALTHCHECK probes the web
// server; a worker has none and would turn "unhealthy".
func TestSpecDisablesImageHealthcheck(t *testing.T) {
	tpl := Template{Image: "img", StacksRoot: "/s", Socket: "/var/run/docker.sock", Jobs: docker.Mount{Type: "bind", Source: "/j"}}
	spec, err := tpl.Spec("01ABC", restic.Repo{})
	if err != nil {
		t.Fatal(err)
	}
	if !spec.NoHealthcheck {
		t.Error("worker spec must set NoHealthcheck")
	}
}

func TestSpecRejectsUnknownRepoVariables(t *testing.T) {
	tpl := Template{Image: "img", StacksRoot: "/s", Socket: "/var/run/docker.sock", Jobs: docker.Mount{Type: "bind", Source: "/j"}}
	if _, err := tpl.Spec("01ABCDEFGHJKMNPQRSTVWXYZ01", restic.Repo{Location: "/r", Env: []string{"LD_PRELOAD=/x"}}); err == nil {
		t.Fatal("want error")
	}
}

func TestJobsDirInStack(t *testing.T) {
	tpl := Template{StacksRoot: "/home/docker", Jobs: docker.Mount{Type: "bind", Source: "/home/docker/stackvault/data/jobs"}}
	for stack, want := range map[string]string{"stackvault": "data/jobs", "web": "", "stack": ""} {
		if got := tpl.JobsDirInStack(stack); got != want {
			t.Errorf("JobsDirInStack(%q) = %q, want %q", stack, got, want)
		}
	}
	tpl.Jobs = docker.Mount{Type: "volume", Source: "stackvault_data", SubPath: "jobs"}
	if got := tpl.JobsDirInStack("stackvault"); got != "" {
		t.Errorf("volume: %q", got)
	}
}

func TestOwnStack(t *testing.T) {
	for _, tt := range []struct {
		root, dir, want string
	}{
		{"/home/docker", "/home/docker/stackvault", "stackvault"},
		{"/home/docker/", "/home/docker/stackvault/", "stackvault"},
		{"/home/docker", "/home/docker/a/b", ""},     // not directly under STACKS_ROOT
		{"/home/docker", "/srv/stackvault", ""},      // elsewhere
		{"/home/docker", "/home/docker/.hidden", ""}, // not a valid stack name
		{"/home/docker", "stackvault", ""},           // relative
		{"/home/docker", "", ""},                     // not started by compose
	} {
		if got := ownStack(tt.root, tt.dir); got != tt.want {
			t.Errorf("ownStack(%q, %q) = %q, want %q", tt.root, tt.dir, got, tt.want)
		}
	}
	cfg := TemplateConfig{StacksRoot: "/home/docker", JobsDir: "/data/jobs"}
	tpl, err := NewTemplate(cfg, &docker.SelfInfo{ImageID: "sha256:self", ComposeWorkingDir: "/home/docker/sv",
		Mounts: []docker.Mount{
			{Type: "bind", Source: "/home/docker", Target: "/home/docker"},
			{Type: "bind", Source: "/home/docker/sv/data", Target: "/data"},
		}})
	if err != nil || tpl.OwnStack != "sv" {
		t.Fatalf("OwnStack = %q, %v", tpl.OwnStack, err)
	}
}

// The manager's docker client configuration (registry credentials) is
// passed read-only to every worker at the same path (issue #14).
func TestTemplateDockerConfig(t *testing.T) {
	cfg := TemplateConfig{StacksRoot: "/home/docker", JobsDir: "/data/jobs", Hostname: "host-a"}
	base := []docker.Mount{
		{Type: "bind", Source: "/home/docker", Target: "/home/docker"},
		{Type: "bind", Source: "/srv/sv/data", Target: "/data"},
	}
	for name, tc := range map[string]struct {
		extra []docker.Mount
		want  *docker.Mount
	}{
		"none":      {nil, nil},
		"directory": {[]docker.Mount{{Type: "bind", Source: "/home/admin/.docker", Target: "/root/.docker"}}, &docker.Mount{Type: "bind", Source: "/home/admin/.docker", Target: "/root/.docker", ReadOnly: true}},
		"file":      {[]docker.Mount{{Type: "bind", Source: "/etc/sv/config.json", Target: "/root/.docker/config.json"}}, &docker.Mount{Type: "bind", Source: "/etc/sv/config.json", Target: "/root/.docker/config.json", ReadOnly: true}},
		"elsewhere": {[]docker.Mount{{Type: "bind", Source: "/x", Target: "/root/other"}}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			tpl, err := NewTemplate(cfg, &docker.SelfInfo{ImageID: "sha256:self", Mounts: append(append([]docker.Mount{}, base...), tc.extra...)})
			if err != nil {
				t.Fatal(err)
			}
			cs, err := tpl.Spec("01ABCDEFGHJKMNPQRSTVWXYZ01", restic.Repo{})
			if err != nil {
				t.Fatal(err)
			}
			var got *docker.Mount
			for _, m := range cs.Mounts {
				if strings.HasPrefix(m.Target, "/root/.docker") {
					got = &m
				}
			}
			if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
				t.Errorf("docker config mount = %+v, want %+v", got, tc.want)
			}
		})
	}
}
