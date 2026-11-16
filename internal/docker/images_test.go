package docker

import (
	"errors"
	"strings"
	"testing"
	"time"
)

const (
	digestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestParseConfiguredServices(t *testing.T) {
	out := []byte(`{"name":"web","services":{
		"app":{"image":"nextcloud:29"},
		"tool":{"build":{"context":"."}},
		"both":{"image":"me/tool:1","build":{"context":"."}}}}`)
	got, err := parseConfiguredServices(out)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]configuredService{
		"app": {Image: "nextcloud:29"}, "tool": {Build: true}, "both": {Image: "me/tool:1", Build: true},
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: %+v, want %+v", k, got[k], v)
		}
	}
}

func TestParseImagetools(t *testing.T) {
	multi := `{"name":"docker.io/library/busybox:1.36","manifest":{"digest":"` + digestA + `"},"image":{
		"linux/amd64":{"created":"2026-01-02T03:04:05Z","os":"linux","architecture":"amd64","config":{"Labels":{"org.opencontainers.image.version":"1.36.1"}}},
		"linux/arm64/v8":{"created":"2026-01-02T03:04:05Z","os":"linux","architecture":"arm64","variant":"v8"},
		"unknown/unknown":"not a config"}}`
	img, err := parseImagetools([]byte(multi))
	if err != nil {
		t.Fatal(err)
	}
	if img.Digest != digestA {
		t.Fatalf("digest %q", img.Digest)
	}
	if c, ok := img.config("linux/amd64"); !ok || c.Config.Labels[VersionLabel] != "1.36.1" {
		t.Fatalf("amd64 config %+v %v", c, ok)
	}
	if _, ok := img.config("linux/arm64"); !ok {
		t.Fatal("linux/arm64 should match linux/arm64/v8")
	}
	if _, ok := img.config("linux/s390x"); ok {
		t.Fatal("unexpected config for s390x")
	}

	single := `{"manifest":{"digest":"` + digestB + `"},"image":{"created":"2026-01-02T03:04:05Z","os":"linux","architecture":"amd64"}}`
	img, err = parseImagetools([]byte(single))
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := img.config("linux/arm64"); !ok || c.Architecture != "amd64" {
		t.Fatalf("single-platform config %+v %v", c, ok)
	}

	if _, err := parseImagetools([]byte(`{"manifest":{}}`)); err == nil {
		t.Fatal("missing digest accepted")
	}
}

func TestEvaluateUpdate(t *testing.T) {
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	running := ServiceState{Name: "app", State: "running", ContainerID: "c1", ImageRef: "nextcloud:29", ImageID: "sha256:1"}
	local := localImage{RepoDigests: []string{"nextcloud@" + digestA}, OS: "linux", Architecture: "amd64", Created: created}
	local.Config.Labels = map[string]string{VersionLabel: "29.0.1"}
	remote := func(d string) remoteImage {
		c := imageConfig{Created: created.Add(time.Hour)}
		c.Config.Labels = map[string]string{VersionLabel: "29.0.2"}
		return remoteImage{Digest: d, Configs: map[string]imageConfig{"linux/amd64": c}}
	}
	cfg := configuredService{Image: "nextcloud:29"}

	tests := []struct {
		name       string
		s          ServiceState
		cfg        configuredService
		local      localImage
		hasLocal   bool
		remote     remoteImage
		remoteErr  error
		want       UpdateStatus
		wantNote   string
		wantFromTo [2]string // current, available version
	}{
		{"up to date", running, cfg, local, true, remote(digestA), nil, UpToDate, "", [2]string{"29.0.1", "29.0.2"}},
		{"update available", running, cfg, local, true, remote(digestB), nil, UpdateAvailable, "", [2]string{"29.0.1", "29.0.2"}},
		{"registry error", running, cfg, local, true, remoteImage{}, errors.New("denied: requested access\nmore"), UpdateUnknown, "registry: denied", [2]string{"29.0.1", ""}},
		{"built locally", running, configuredService{Build: true}, local, true, remoteImage{}, nil, NotChecked, "built locally", [2]string{}},
		{"pinned", running, configuredService{Image: "nextcloud@" + digestA}, local, true, remoteImage{}, nil, NotChecked, "pinned", [2]string{}},
		{"compose file changed", running, configuredService{Image: "nextcloud:30"}, local, true, remote(digestB), nil, UpdateAvailable, "another image", [2]string{}},
		{"pinned by a restore", ServiceState{Name: "app", State: "running", ContainerID: "c1", ImageRef: "nextcloud@" + digestA}, cfg, local, true, remote(digestA), nil, UpToDate, "pinned by digest by a restore", [2]string{}},
		{"pinned by a restore, newer exists", ServiceState{Name: "app", State: "running", ContainerID: "c1", ImageRef: "nextcloud@" + digestA}, cfg, local, true, remote(digestB), nil, UpdateAvailable, "pinned by digest by a restore", [2]string{}},
		{"no digest", running, cfg, localImage{}, true, remote(digestB), nil, UpdateUnknown, "no registry digest", [2]string{}},
		{"no container", ServiceState{Name: "app"}, cfg, localImage{}, false, remote(digestB), nil, UpdateUnknown, "no container", [2]string{}},
		{"stopped", ServiceState{Name: "app", State: "exited", ContainerID: "c1", ImageRef: "nextcloud:29"}, cfg, local, true, remote(digestB), nil, UpdateAvailable, "not running", [2]string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evaluateUpdate(tt.s, tt.cfg, tt.local, tt.hasLocal, tt.remote, tt.remoteErr)
			if got.Status != tt.want || !strings.Contains(got.Note, tt.wantNote) {
				t.Fatalf("status %q note %q, want %q / %q", got.Status, got.Note, tt.want, tt.wantNote)
			}
			if tt.wantFromTo != [2]string{} && (got.Current.Version != tt.wantFromTo[0] || got.Available.Version != tt.wantFromTo[1]) {
				t.Errorf("versions %q -> %q", got.Current.Version, got.Available.Version)
			}
			if got.Pinned != strings.HasPrefix(tt.name, "pinned by a restore") {
				t.Errorf("pinned = %v", got.Pinned)
			}
			if got.Status == UpdateAvailable && got.Current.Digest == got.Available.Digest {
				t.Errorf("same digest for an available update: %+v", got)
			}
		})
	}
}

func TestRepository(t *testing.T) {
	for ref, want := range map[string]string{
		"nextcloud:29": "nextcloud", "nextcloud@" + digestA: "nextcloud", "nextcloud": "nextcloud",
		"host:5000/app:1": "host:5000/app", "host:5000/app": "host:5000/app", "ghcr.io/a/b:v1@" + digestA: "ghcr.io/a/b",
	} {
		if got := repository(ref); got != want {
			t.Errorf("repository(%q) = %q, want %q", ref, got, want)
		}
	}
}
