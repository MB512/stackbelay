package config

import (
	"io/fs"
	"strings"
	"testing"
	"time"
)

const testKey = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=" // 32 bytes

func noFiles(string) ([]byte, error) { return nil, fs.ErrNotExist }

func TestLoadManagerDefaults(t *testing.T) {
	c, err := LoadManager(lookup(map[string]string{
		"STACKS_ROOT":           "/home/docker",
		"STACKVAULT_HOSTNAME":   "host-a",
		"STACKVAULT_SECRET_KEY": testKey,
	}), noFiles)
	if err != nil {
		t.Fatal(err)
	}
	if c.DataDir != "/data" || c.ListenAddr != ":8080" || c.SessionTTL != 12*time.Hour ||
		c.AllowInsecureHTTP || c.WorkerImage != "" || len(c.SecretKey) != 32 || c.MaxJobs != 1 || c.EnableShell ||
		c.CleanupImageMinAge != 168*time.Hour {
		t.Fatalf("unexpected config %+v", c)
	}
	if c.DBPath() != "/data/stackvault.db" || c.JobsDir() != "/data/jobs" {
		t.Fatalf("paths: %s %s", c.DBPath(), c.JobsDir())
	}
}

func TestLoadManagerWithoutKeySource(t *testing.T) {
	c, err := LoadManager(lookup(map[string]string{
		"STACKS_ROOT":         "/home/docker",
		"STACKVAULT_HOSTNAME": "host-a",
	}), noFiles)
	if err != nil {
		t.Fatal(err)
	}
	if c.SecretKey != nil || c.SecretKeyPath() != "/data/secret.key" {
		t.Fatalf("SecretKey %v, SecretKeyPath %s", c.SecretKey, c.SecretKeyPath())
	}
}

func TestLoadManagerKeyFile(t *testing.T) {
	read := func(p string) ([]byte, error) {
		if p == "/run/secrets/key" {
			return []byte(testKey + "\n"), nil
		}
		return nil, fs.ErrNotExist
	}
	c, err := LoadManager(lookup(map[string]string{
		"STACKS_ROOT":                      "/home/docker",
		"STACKVAULT_HOSTNAME":              "host-a",
		"STACKVAULT_SECRET_KEY_FILE":       "/run/secrets/key",
		"STACKVAULT_DATA_DIR":              "/srv/sv/",
		"STACKVAULT_ALLOW_INSECURE_HTTP":   "true",
		"STACKVAULT_SESSION_TTL":           "30m",
		"STACKVAULT_MAX_JOBS":              "3",
		"STACKVAULT_ENABLE_SHELL":          "true",
		"STACKVAULT_CLEANUP_IMAGE_MIN_AGE": "48h",
		"TZ":                               ":Europe/Berlin",
	}), read)
	if err != nil {
		t.Fatal(err)
	}
	if c.TimeZone == nil || c.TimeZone.String() != "Europe/Berlin" {
		t.Fatalf("time zone %v", c.TimeZone)
	}
	if len(c.SecretKey) != 32 || c.DataDir != "/srv/sv" || !c.AllowInsecureHTTP || c.SessionTTL != 30*time.Minute || c.MaxJobs != 3 || !c.EnableShell ||
		c.CleanupImageMinAge != 48*time.Hour {
		t.Fatalf("unexpected config %+v", c)
	}
}

func TestLoadManagerErrors(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr []string
	}{
		{"nothing set", map[string]string{}, []string{"STACKS_ROOT", "STACKVAULT_HOSTNAME"}},
		{"both keys", map[string]string{"STACKVAULT_SECRET_KEY": testKey, "STACKVAULT_SECRET_KEY_FILE": "/k"}, []string{"only one"}},
		{"short key", map[string]string{"STACKVAULT_SECRET_KEY": "c2hvcnQ="}, []string{"32 bytes"}},
		{"missing key file", map[string]string{"STACKVAULT_SECRET_KEY_FILE": "/nope"}, []string{"STACKVAULT_SECRET_KEY_FILE"}},
		{"relative data dir", map[string]string{"STACKVAULT_DATA_DIR": "data"}, []string{"STACKVAULT_DATA_DIR must be an absolute path"}},
		{"bad bool", map[string]string{"STACKVAULT_ALLOW_INSECURE_HTTP": "maybe"}, []string{"STACKVAULT_ALLOW_INSECURE_HTTP"}},
		{"bad ttl", map[string]string{"STACKVAULT_SESSION_TTL": "5s"}, []string{"STACKVAULT_SESSION_TTL"}},
		{"bad max jobs", map[string]string{"STACKVAULT_MAX_JOBS": "0"}, []string{"STACKVAULT_MAX_JOBS"}},
		{"bad cleanup age", map[string]string{"STACKVAULT_CLEANUP_IMAGE_MIN_AGE": "90m"}, []string{"STACKVAULT_CLEANUP_IMAGE_MIN_AGE"}},
		{"negative cleanup age", map[string]string{"STACKVAULT_CLEANUP_IMAGE_MIN_AGE": "-1h"}, []string{"STACKVAULT_CLEANUP_IMAGE_MIN_AGE"}},
		{"bad shell flag", map[string]string{"STACKVAULT_ENABLE_SHELL": "maybe"}, []string{"STACKVAULT_ENABLE_SHELL"}},
		{"unknown time zone", map[string]string{"TZ": "Europe/Atlantis"}, []string{"TZ:"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadManager(lookup(tt.env), noFiles)
			if err == nil {
				t.Fatal("expected error")
			}
			for _, w := range tt.wantErr {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error does not mention %q: %v", w, err)
				}
			}
			if strings.Contains(err.Error(), testKey) {
				t.Error("error contains the key")
			}
		})
	}
}
