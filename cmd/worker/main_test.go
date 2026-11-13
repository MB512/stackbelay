package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunUsageAndConfigErrors(t *testing.T) {
	root := t.TempDir()
	env := map[string]string{
		"STACKS_ROOT":          root,
		"STACKVAULT_HOSTNAME":  "h",
		"RESTIC_REPOSITORY":    "/nonexistent",
		"RESTIC_PASSWORD_FILE": "/nonexistent",
	}
	getenv := func(k string) string { return env[k] }

	tests := []struct {
		name     string
		args     []string
		getenv   func(string) string
		wantCode int
		wantOut  string
	}{
		{"no config", []string{"backup", "x"}, func(string) string { return "" }, 2, "STACKS_ROOT is required"},
		{"no command", nil, getenv, 2, "missing command"},
		{"unknown command", []string{"prune"}, getenv, 2, "unknown command"},
		{"backup without stack", []string{"backup"}, getenv, 2, "backup expects"},
		{"restore without snapshot", []string{"restore", "web"}, getenv, 2, "restore expects"},
		{"unknown stack", []string{"backup", "../etc"}, getenv, 1, "stack not found"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			code := run(tt.args, tt.getenv, &out)
			if code != tt.wantCode {
				t.Fatalf("exit code %d, want %d; output: %s", code, tt.wantCode, out.String())
			}
			if !strings.Contains(out.String(), tt.wantOut) {
				t.Fatalf("output %q does not contain %q", out.String(), tt.wantOut)
			}
		})
	}
}
