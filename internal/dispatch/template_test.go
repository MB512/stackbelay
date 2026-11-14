package dispatch

import (
	"strings"
	"testing"

	"github.com/MB512/stackvault/internal/docker"
	"github.com/MB512/stackvault/internal/restic"
)

func TestNewTemplate(t *testing.T) {
	cfg := TemplateConfig{StacksRoot: "/home/docker", JobsDir: "/data/jobs", Hostname: "host-a"}
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
	cfg.WorkerImage = "ghcr.io/x/stackvault:1"
	tpl, err = NewTemplate(cfg, &docker.SelfInfo{ImageID: "sha256:self", Mounts: []docker.Mount{
		stacksMount, {Type: "volume", Source: "stackvault_data", Target: "/data"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if tpl.Image != "ghcr.io/x/stackvault:1" || tpl.Socket != "/var/run/docker.sock" ||
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
