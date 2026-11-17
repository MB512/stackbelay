package docker

import (
	"reflect"
	"strings"
	"testing"
)

func TestRunArgs(t *testing.T) {
	spec := ContainerSpec{
		Name:   "stackvault-job-01ABC",
		Image:  "sha256:abc",
		Labels: map[string]string{"stackvault.worker": "true", "stackvault.job": "01ABC"},
		Mounts: []Mount{
			{Type: "bind", Source: "/home/docker", Target: "/home/docker"},
			{Type: "bind", Source: "/var/run/docker.sock", Target: "/var/run/docker.sock"},
			{Type: "volume", Source: "stackvault_data", Target: "/job", SubPath: "jobs/01ABC"},
			{Type: "volume", Source: "stackvault-restic-cache", Target: "/cache", ReadOnly: false},
		},
		Env:           map[string]string{"RESTIC_PASSWORD": "s3cret", "RESTIC_REPOSITORY": "/r"},
		Cmd:           []string{"/app/worker", "run", "/job"},
		NoHealthcheck: true,
	}
	args, err := runArgs(spec)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"run", "--detach", "--name", "stackvault-job-01ABC",
		"--label", "stackvault.job=01ABC", "--label", "stackvault.worker=true",
		"--mount", "type=bind,source=/home/docker,target=/home/docker",
		"--mount", "type=bind,source=/var/run/docker.sock,target=/var/run/docker.sock",
		"--mount", "type=volume,source=stackvault_data,target=/job,volume-subpath=jobs/01ABC",
		"--mount", "type=volume,source=stackvault-restic-cache,target=/cache",
		"--env", "RESTIC_PASSWORD", "--env", "RESTIC_REPOSITORY",
		"--no-healthcheck",
		"sha256:abc", "/app/worker", "run", "/job"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args:\n got %q\nwant %q", args, want)
	}
	if strings.Contains(strings.Join(args, " "), "s3cret") {
		t.Fatal("secret value on the command line")
	}
}

func TestRunArgsRejects(t *testing.T) {
	base := func() ContainerSpec {
		return ContainerSpec{Name: "n", Image: "img", Cmd: []string{"x"}}
	}
	tests := map[string]func(*ContainerSpec){
		"no image":         func(s *ContainerSpec) { s.Image = "" },
		"image as flag":    func(s *ContainerSpec) { s.Image = "--privileged" },
		"comma in path":    func(s *ContainerSpec) { s.Mounts = []Mount{{Type: "bind", Source: "/a,type=bind", Target: "/b"}} },
		"relative bind":    func(s *ContainerSpec) { s.Mounts = []Mount{{Type: "bind", Source: "a", Target: "/b"}} },
		"relative target":  func(s *ContainerSpec) { s.Mounts = []Mount{{Type: "bind", Source: "/a", Target: "b"}} },
		"unknown type":     func(s *ContainerSpec) { s.Mounts = []Mount{{Type: "tmpfs", Source: "x", Target: "/b"}} },
		"bad env name":     func(s *ContainerSpec) { s.Env = map[string]string{"A=B": "x"} },
		"docker env clash": func(s *ContainerSpec) { s.Env = map[string]string{"DOCKER_HOST": "tcp://evil"} },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			s := base()
			mutate(&s)
			if _, err := runArgs(s); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestParseSelf(t *testing.T) {
	out := []byte(`{"Image":"sha256:feed","Config":{"Labels":{"com.docker.compose.project.working_dir":"/srv/stacks/stackvault"}},"Mounts":[
		{"Type":"bind","Source":"/home/docker","Destination":"/home/docker","RW":true},
		{"Type":"volume","Name":"stackvault_data","Source":"/var/lib/docker/volumes/stackvault_data/_data","Destination":"/data","RW":true},
		{"Type":"bind","Source":"/var/run/docker.sock","Destination":"/var/run/docker.sock","RW":false},
		{"Type":"tmpfs","Destination":"/tmp"}]}`)
	info, err := parseSelf(out)
	if err != nil {
		t.Fatal(err)
	}
	want := SelfInfo{ImageID: "sha256:feed", ComposeWorkingDir: "/srv/stacks/stackvault", Mounts: []Mount{
		{Type: "bind", Source: "/home/docker", Target: "/home/docker"},
		{Type: "volume", Source: "stackvault_data", Target: "/data"},
		{Type: "bind", Source: "/var/run/docker.sock", Target: "/var/run/docker.sock", ReadOnly: true},
	}}
	if !reflect.DeepEqual(info, want) {
		t.Fatalf("got %+v", info)
	}
}

func TestOwnContainerID(t *testing.T) {
	id := strings.Repeat("ab12", 16)
	mountinfo := "1 0 0:1 / / rw - overlay overlay rw\n" +
		"2 1 8:1 /var/lib/docker/containers/" + id + "/resolv.conf /etc/resolv.conf rw - ext4 /dev/sda1 rw\n"
	if got := OwnContainerID([]byte(mountinfo)); got != id {
		t.Fatalf("got %q", got)
	}
	if got := OwnContainerID([]byte("1 0 0:1 / / rw - ext4 /dev/sda1 rw\n")); got != "" {
		t.Fatalf("outside a container: %q", got)
	}
}

func TestContainerStateRunning(t *testing.T) {
	for status, want := range map[string]bool{"created": true, "running": true, "restarting": true, "exited": false, "dead": false} {
		if got := (ContainerState{Exists: true, Status: status}).Running(); got != want {
			t.Errorf("%s: %v", status, got)
		}
	}
	if (ContainerState{}).Running() {
		t.Error("missing container reported running")
	}
}
