package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MB512/stackbelay/internal/config"
	"github.com/MB512/stackbelay/internal/secrets"
	"github.com/MB512/stackbelay/internal/store"
)

// openOnce opens and closes the store like a manager start and stop.
func openOnce(t *testing.T, cfg config.Manager, logs *bytes.Buffer) error {
	t.Helper()
	st, err := openStore(context.Background(), cfg, slog.New(slog.NewTextHandler(logs, nil)))
	if err == nil {
		_ = st.Close()
	}
	return err
}

func readKey(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func TestFreshStartCreatesKeyAndReusesIt(t *testing.T) {
	cfg := config.Manager{DataDir: t.TempDir()}
	var logs bytes.Buffer
	if err := openOnce(t, cfg, &logs); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(cfg.SecretKeyPath())
	if err != nil {
		t.Fatalf("key file not created: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("key file mode %v, want 0600", perm)
	}
	key := readKey(t, cfg.SecretKeyPath())
	if strings.Contains(logs.String(), key) {
		t.Fatal("the key was logged")
	}

	if err := openOnce(t, cfg, &logs); err != nil {
		t.Fatalf("second start: %v", err)
	}
	if readKey(t, cfg.SecretKeyPath()) != key {
		t.Fatal("second start replaced the key")
	}
}

func TestStartRefusedWhenDatabaseExistsWithoutKey(t *testing.T) {
	cfg := config.Manager{DataDir: t.TempDir()}
	var logs bytes.Buffer
	if err := openOnce(t, cfg, &logs); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(cfg.SecretKeyPath()); err != nil {
		t.Fatal(err)
	}
	err := openOnce(t, cfg, &logs)
	if err == nil || !strings.Contains(err.Error(), "no secret key") {
		t.Fatalf("err = %v, want refusal", err)
	}
	if _, err := os.Stat(cfg.SecretKeyPath()); err == nil {
		t.Fatal("a new key was created next to an existing database")
	}
}

func TestStartRefusedWithWrongKey(t *testing.T) {
	cfg := config.Manager{DataDir: t.TempDir()}
	var logs bytes.Buffer
	if err := openOnce(t, cfg, &logs); err != nil {
		t.Fatal(err)
	}
	other, err := secrets.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.SecretKeyPath(), []byte(other+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = openOnce(t, cfg, &logs)
	if !errors.Is(err, store.ErrKeyMismatch) || !strings.Contains(err.Error(), "does not match this database") {
		t.Fatalf("err = %v, want ErrKeyMismatch", err)
	}
}

func TestExplicitKeyIgnoresDataDirKey(t *testing.T) {
	cfg := config.Manager{DataDir: t.TempDir()}
	enc, err := secrets.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SecretKey, err = secrets.ParseKey(enc); err != nil {
		t.Fatal(err)
	}
	// A data-dir key that differs from the explicit one, and is not even valid.
	if err := os.WriteFile(cfg.SecretKeyPath(), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	if err := openOnce(t, cfg, &logs); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "ignoring the key file") {
		t.Errorf("no warning about the ignored key file: %s", logs.String())
	}
	// The database was encrypted with the explicit key: it opens again with it.
	if err := openOnce(t, cfg, &logs); err != nil {
		t.Fatalf("second start: %v", err)
	}
	if got := readKey(t, cfg.SecretKeyPath()); got != "garbage" {
		t.Fatal("the data-dir key file was changed")
	}
}

func TestExplicitKeyOnFreshInstallCreatesNoKeyFile(t *testing.T) {
	cfg := config.Manager{DataDir: t.TempDir()}
	enc, _ := secrets.GenerateKey()
	cfg.SecretKey, _ = secrets.ParseKey(enc)
	var logs bytes.Buffer
	if err := openOnce(t, cfg, &logs); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(cfg.DataDir, secrets.KeyFileName)); err == nil {
		t.Fatal("key file created although an explicit key is set")
	}
}
