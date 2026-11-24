package docker

import (
	"errors"
	"testing"
)

func TestNormalizeRef(t *testing.T) {
	const d = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	for ref, want := range map[string]string{
		"":                                     "",
		"nginx":                                "docker.io/library/nginx:latest",
		"nginx:latest":                         "docker.io/library/nginx:latest",
		"library/nginx:latest":                 "docker.io/library/nginx:latest",
		"docker.io/library/nginx:latest":       "docker.io/library/nginx:latest",
		"index.docker.io/library/nginx":        "docker.io/library/nginx:latest",
		"zabbix/zabbix-agent":                  "docker.io/zabbix/zabbix-agent:latest",
		"docker.io/zabbix/zabbix-agent:latest": "docker.io/zabbix/zabbix-agent:latest",
		"nginx:1.27":                           "docker.io/library/nginx:1.27",
		"host:5000/app":                        "host:5000/app:latest",
		"host:5000/app:1":                      "host:5000/app:1",
		"localhost/app":                        "localhost/app:latest",
		"Registry.Example.com/team/app":        "registry.example.com/team/app:latest",
		"ghcr.io/owner/app":                    "ghcr.io/owner/app:latest",
		"name@" + d:                            "docker.io/library/name@" + d,
		"name:tag@" + d:                        "docker.io/library/name:tag@" + d,
		"ghcr.io/owner/app@" + d:               "ghcr.io/owner/app@" + d,
	} {
		if got := NormalizeRef(ref); got != want {
			t.Errorf("NormalizeRef(%q) = %q, want %q", ref, got, want)
		}
	}
	if !SameRef("nginx", "docker.io/library/nginx:latest") || SameRef("app:1", "app:2") || SameRef("nginx", "nginx@"+d) {
		t.Error("SameRef")
	}
	if got := RefRepository("docker.io/library/nginx@" + d); got != "docker.io/library/nginx" || RefRepository("nginx:1") != got {
		t.Errorf("RefRepository = %q", got)
	}
}

func TestPinnedByRestore(t *testing.T) {
	const d = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	tests := []struct {
		running, configured string
		want                bool
	}{
		{"nginx@" + d, "nginx:1.27", true},
		{"docker.io/library/nginx@" + d, "nginx", true}, // issue #40
		{"nginx:1.27", "nginx:1.27", false},
		{"nginx", "nginx:latest", false},
		{"redis@" + d, "nginx", false},
	}
	for _, tt := range tests {
		if got := pinnedByRestore(tt.running, tt.configured); got != tt.want {
			t.Errorf("pinnedByRestore(%q, %q) = %v", tt.running, tt.configured, got)
		}
	}
}

func TestIsImageNotFound(t *testing.T) {
	for msg, want := range map[string]bool{
		"docker pull: exit status 1: Error response from daemon: manifest for nginx@sha256:ab not found: manifest unknown: manifest unknown": true,
		"Error response from daemon: manifest unknown":                                                true,
		"Error response from daemon: Get \"https://registry-1.docker.io/v2/\": dial tcp: i/o timeout": false,
		"pull access denied for x, repository does not exist or may require 'docker login': denied":   false,
		"Error response from daemon: unauthorized: authentication required":                           false,
	} {
		if got := IsImageNotFound(errors.New(msg)); got != want {
			t.Errorf("IsImageNotFound(%q) = %v", msg, got)
		}
	}
	if IsImageNotFound(nil) {
		t.Error("nil")
	}
}
