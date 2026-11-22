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
		"STACKBELAY_HOSTNAME":   "host-a",
		"STACKBELAY_SECRET_KEY": testKey,
	}), noFiles)
	if err != nil {
		t.Fatal(err)
	}
	if c.DataDir != "/data" || c.ListenAddr != ":8080" || c.SessionTTL != 12*time.Hour ||
		c.AllowInsecureHTTP || c.WorkerImage != "" || len(c.SecretKey) != 32 || c.MaxJobs != 3 || c.EnableShell ||
		c.CleanupImageMinAge != 168*time.Hour || c.JobTimeout != 48*time.Hour || c.MaxDowntimes != 1 ||
		c.DowntimeTimeout != time.Hour || c.JobLogRetention != 90*24*time.Hour {
		t.Fatalf("unexpected config %+v", c)
	}
	if c.DBPath() != "/data/stackvault.db" || c.JobsDir() != "/data/jobs" {
		t.Fatalf("paths: %s %s", c.DBPath(), c.JobsDir())
	}
}

func TestLoadManagerWithoutKeySource(t *testing.T) {
	c, err := LoadManager(lookup(map[string]string{
		"STACKS_ROOT":         "/home/docker",
		"STACKBELAY_HOSTNAME": "host-a",
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
		"STACKBELAY_HOSTNAME":              "host-a",
		"STACKBELAY_SECRET_KEY_FILE":       "/run/secrets/key",
		"STACKBELAY_DATA_DIR":              "/srv/sv/",
		"STACKBELAY_ALLOW_INSECURE_HTTP":   "true",
		"STACKBELAY_SESSION_TTL":           "30m",
		"STACKBELAY_MAX_JOBS":              "5",
		"STACKBELAY_ENABLE_SHELL":          "true",
		"STACKBELAY_CLEANUP_IMAGE_MIN_AGE": "48h",
		"STACKBELAY_JOB_TIMEOUT":           "72h",
		"STACKBELAY_MAX_DOWNTIMES":         "2",
		"STACKBELAY_DOWNTIME_TIMEOUT":      "30m",
		"TZ":                               ":Europe/Berlin",
	}), read)
	if err != nil {
		t.Fatal(err)
	}
	if c.TimeZone == nil || c.TimeZone.String() != "Europe/Berlin" {
		t.Fatalf("time zone %v", c.TimeZone)
	}
	if len(c.SecretKey) != 32 || c.DataDir != "/srv/sv" || !c.AllowInsecureHTTP || c.SessionTTL != 30*time.Minute || c.MaxJobs != 5 || !c.EnableShell ||
		c.CleanupImageMinAge != 48*time.Hour || c.JobTimeout != 72*time.Hour || c.MaxDowntimes != 2 ||
		c.DowntimeTimeout != 30*time.Minute {
		t.Fatalf("unexpected config %+v", c)
	}
}

func TestLoadManagerErrors(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr []string
	}{
		{"nothing set", map[string]string{}, []string{"STACKS_ROOT", "STACKBELAY_HOSTNAME"}},
		{"both keys", map[string]string{"STACKBELAY_SECRET_KEY": testKey, "STACKBELAY_SECRET_KEY_FILE": "/k"}, []string{"only one"}},
		{"short key", map[string]string{"STACKBELAY_SECRET_KEY": "c2hvcnQ="}, []string{"32 bytes"}},
		{"missing key file", map[string]string{"STACKBELAY_SECRET_KEY_FILE": "/nope"}, []string{"STACKBELAY_SECRET_KEY_FILE"}},
		{"relative data dir", map[string]string{"STACKBELAY_DATA_DIR": "data"}, []string{"STACKBELAY_DATA_DIR must be an absolute path"}},
		{"bad bool", map[string]string{"STACKBELAY_ALLOW_INSECURE_HTTP": "maybe"}, []string{"STACKBELAY_ALLOW_INSECURE_HTTP"}},
		{"bad ttl", map[string]string{"STACKBELAY_SESSION_TTL": "5s"}, []string{"STACKBELAY_SESSION_TTL"}},
		{"bad max jobs", map[string]string{"STACKBELAY_MAX_JOBS": "0"}, []string{"STACKBELAY_MAX_JOBS"}},
		{"short log retention", map[string]string{"STACKBELAY_JOB_LOG_RETENTION": "1h"}, []string{"STACKBELAY_JOB_LOG_RETENTION"}},
		{"bad max downtimes", map[string]string{"STACKBELAY_MAX_DOWNTIMES": "33"}, []string{"STACKBELAY_MAX_DOWNTIMES"}},
		{"bad downtime timeout", map[string]string{"STACKBELAY_DOWNTIME_TIMEOUT": "10s"}, []string{"STACKBELAY_DOWNTIME_TIMEOUT"}},
		{"bad cleanup age", map[string]string{"STACKBELAY_CLEANUP_IMAGE_MIN_AGE": "90m"}, []string{"STACKBELAY_CLEANUP_IMAGE_MIN_AGE"}},
		{"negative cleanup age", map[string]string{"STACKBELAY_CLEANUP_IMAGE_MIN_AGE": "-1h"}, []string{"STACKBELAY_CLEANUP_IMAGE_MIN_AGE"}},
		{"bad shell flag", map[string]string{"STACKBELAY_ENABLE_SHELL": "maybe"}, []string{"STACKBELAY_ENABLE_SHELL"}},
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
