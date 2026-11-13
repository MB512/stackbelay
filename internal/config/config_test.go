package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

func lookup(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadWorker(t *testing.T) {
	c, err := LoadWorker(lookup(map[string]string{
		"STACKS_ROOT":            "/home/docker/",
		"STACKVAULT_HOSTNAME":    "host-a",
		"RESTIC_REPOSITORY":      "rest:https://backup.example/web",
		"RESTIC_PASSWORD_FILE":   "/run/secrets/pw",
		"AWS_SECRET_ACCESS_KEY":  "aws-secret",
		"STACKVAULT_JOB_TIMEOUT": "30m",
		"STACKVAULT_LOG_LEVEL":   "debug",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.StacksRoot != "/home/docker" || c.Hostname != "host-a" || c.JobTimeout != 30*time.Minute ||
		c.LogLevel != slog.LevelDebug || c.RepoLocation != "rest:https://backup.example/web" {
		t.Fatalf("unexpected config %+v", c)
	}
	if len(c.RepoSecretEnv) != 1 || c.RepoSecretEnv[0] != "AWS_SECRET_ACCESS_KEY=aws-secret" {
		t.Fatalf("unexpected secret env %v", c.RepoSecretEnv)
	}
}

func TestLoadWorkerErrors(t *testing.T) {
	_, err := LoadWorker(lookup(map[string]string{
		"STACKS_ROOT":            "relative",
		"STACKVAULT_JOB_TIMEOUT": "forever",
		"STACKVAULT_LOG_LEVEL":   "loud",
	}))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"STACKS_ROOT", "RESTIC_REPOSITORY", "RESTIC_PASSWORD", "STACKVAULT_JOB_TIMEOUT", "STACKVAULT_LOG_LEVEL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
}
