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
	want := []string{"RESTIC_PASSWORD_FILE=/run/secrets/pw", "AWS_SECRET_ACCESS_KEY=aws-secret"}
	if strings.Join(c.RepoEnv, "|") != strings.Join(want, "|") {
		t.Fatalf("RepoEnv = %v, want %v", c.RepoEnv, want)
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
	for _, want := range []string{"STACKS_ROOT", "STACKVAULT_JOB_TIMEOUT", "STACKVAULT_LOG_LEVEL"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
}

func TestLoadWorkerRepository(t *testing.T) {
	base := map[string]string{"STACKS_ROOT": "/home/docker", "STACKVAULT_HOSTNAME": "h"}
	with := func(kv ...string) map[string]string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	tests := []struct {
		name    string
		env     map[string]string
		hasRepo bool
		wantErr string
	}{
		{"no repository (update without backup)", with(), false, ""},
		{"repository with password file", with("RESTIC_REPOSITORY", "/r", "RESTIC_PASSWORD_FILE", "/pw"), true, ""},
		{"repository without password", with("RESTIC_REPOSITORY", "/r"), false, "RESTIC_PASSWORD"},
		{"password without repository", with("RESTIC_PASSWORD", "x"), false, "RESTIC_REPOSITORY"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := LoadWorker(lookup(tt.env))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || c.HasRepo() != tt.hasRepo {
				t.Fatalf("HasRepo %v, err %v", c.HasRepo(), err)
			}
		})
	}
}

func TestJobTimeout(t *testing.T) {
	tests := []struct {
		value   string
		want    time.Duration
		wantErr bool
	}{
		{"", DefaultJobTimeout, false},
		{"72h", 72 * time.Hour, false},
		{"1m", time.Minute, false},
		{"30s", DefaultJobTimeout, true},
		{"-1h", DefaultJobTimeout, true},
		{"forever", DefaultJobTimeout, true},
	}
	for _, tt := range tests {
		got, errs := jobTimeout(lookup(map[string]string{"STACKVAULT_JOB_TIMEOUT": tt.value}), nil)
		if got != tt.want || (len(errs) > 0) != tt.wantErr {
			t.Errorf("%q: got %v, errors %v; want %v, error %v", tt.value, got, errs, tt.want, tt.wantErr)
		}
	}
}
