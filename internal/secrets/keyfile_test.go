package secrets

import (
	"bytes"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadOrCreateKeyFileCreatesAndReuses(t *testing.T) {
	path := filepath.Join(t.TempDir(), KeyFileName)
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))

	key, created, err := LoadOrCreateKeyFile(path, true, log)
	if err != nil || !created || len(key) != KeySize {
		t.Fatalf("first call: key %d bytes, created %v, err %v", len(key), created, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode %v, want 0600", perm)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	enc := strings.TrimSpace(string(data))
	if strings.Contains(logs.String(), enc) {
		t.Fatal("the key was logged")
	}
	if !strings.Contains(logs.String(), path) {
		t.Errorf("path not logged: %s", logs.String())
	}

	again, created, err := LoadOrCreateKeyFile(path, true, log)
	if err != nil || created || !bytes.Equal(again, key) {
		t.Fatalf("second call: same key %v, created %v, err %v", bytes.Equal(again, key), created, err)
	}
}

func TestLoadOrCreateKeyFileMissingWithoutCreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), KeyFileName)
	_, _, err := LoadOrCreateKeyFile(path, false, slog.New(slog.DiscardHandler))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want fs.ErrNotExist", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("key file was created")
	}
}

func TestLoadOrCreateKeyFileExisting(t *testing.T) {
	const testKey = "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=" // 32 bytes
	tests := []struct {
		name     string
		content  string
		mode     fs.FileMode
		wantErr  string
		wantWarn bool
	}{
		{"valid with newline", testKey + "\n", 0o600, "", false},
		{"world readable", testKey, 0o644, "", true},
		{"group readable", testKey, 0o640, "", true},
		{"short key", "c2hvcnQ=", 0o600, "32 bytes", false},
		{"not base64", "not a key!", 0o600, "base64", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), KeyFileName)
			if err := os.WriteFile(path, []byte(tt.content), tt.mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, tt.mode); err != nil { // umask may have removed bits
				t.Fatal(err)
			}
			var logs bytes.Buffer
			key, created, err := LoadOrCreateKeyFile(path, true, slog.New(slog.NewTextHandler(&logs, nil)))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				data, _ := os.ReadFile(path)
				if string(data) != tt.content {
					t.Fatal("invalid key file was overwritten")
				}
				return
			}
			if err != nil || created || len(key) != KeySize {
				t.Fatalf("key %d bytes, created %v, err %v", len(key), created, err)
			}
			if warned := strings.Contains(logs.String(), "level=WARN"); warned != tt.wantWarn {
				t.Errorf("warning = %v, want %v: %s", warned, tt.wantWarn, logs.String())
			}
		})
	}
}
