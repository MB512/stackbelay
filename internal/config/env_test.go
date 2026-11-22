package config

import (
	"bytes"
	"log/slog"
	"slices"
	"strings"
	"testing"
)

func TestEnvFallsBackToLegacyNames(t *testing.T) {
	vars := map[string]string{
		"STACKBELAY_LISTEN":   ":9000",
		"STACKVAULT_LISTEN":   ":7000", // ignored: the new name is set
		"STACKVAULT_HOSTNAME": "old-host",
		"STACKVAULT_MAX_JOBS": "",
		"OTHER":               "x",
		"STACKVAULT_OTHER":    "never read",
	}
	env := NewEnv(func(k string) string { return vars[k] })

	tests := []struct{ name, want string }{
		{"STACKBELAY_LISTEN", ":9000"},
		{"STACKBELAY_HOSTNAME", "old-host"},
		{"STACKBELAY_HOSTNAME", "old-host"}, // read twice, listed once
		{"STACKBELAY_MAX_JOBS", ""},
		{"STACKBELAY_LOG_LEVEL", ""},
		{"OTHER", "x"},
		{"MISSING", ""},
	}
	for _, tt := range tests {
		if got := env.Get(tt.name); got != tt.want {
			t.Errorf("Get(%q) = %q, want %q", tt.name, got, tt.want)
		}
	}
	if got, want := env.Legacy(), []string{"STACKVAULT_HOSTNAME"}; !slices.Equal(got, want) {
		t.Errorf("Legacy() = %v, want %v", got, want)
	}

	var buf bytes.Buffer
	env.WarnLegacy(slog.New(slog.NewTextHandler(&buf, nil)))
	out := buf.String()
	if strings.Count(out, "level=WARN") != 1 || !strings.Contains(out, "name=STACKVAULT_HOSTNAME") ||
		!strings.Contains(out, "new_name=STACKBELAY_HOSTNAME") {
		t.Errorf("WarnLegacy logged %q", out)
	}
}

func TestLoadManagerReadsLegacyNames(t *testing.T) {
	vars := map[string]string{
		"STACKVAULT_HOSTNAME": "old-host",
		"STACKVAULT_MAX_JOBS": "5",
		"STACKS_ROOT":         "/home/docker",
	}
	env := NewEnv(func(k string) string { return vars[k] })
	cfg, err := LoadManager(env.Get, func(string) ([]byte, error) { return nil, nil })
	if err != nil {
		t.Fatalf("LoadManager: %v", err)
	}
	if cfg.Hostname != "old-host" || cfg.MaxJobs != 5 {
		t.Errorf("Hostname = %q, MaxJobs = %d; want old-host, 5", cfg.Hostname, cfg.MaxJobs)
	}
}
