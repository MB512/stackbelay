package secrets

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
)

// KeyFileName is the key file in the data directory, used when neither
// STACKVAULT_SECRET_KEY nor STACKVAULT_SECRET_KEY_FILE is set.
const KeyFileName = "secret.key"

// maxKeyFileSize bounds how much of a key file is read; a valid key file
// is 45 bytes (44 base64 characters and a newline).
const maxKeyFileSize = 1024

// LoadOrCreateKeyFile returns the key stored in the file at path (base64 of
// KeySize bytes, surrounding whitespace ignored, as in STACKVAULT_SECRET_KEY).
//
// If the file does not exist and create is true, a new random key is
// written there and created is true. The file is created exclusively
// (never overwriting an existing one) with mode 0600, and both the file and
// its directory are synced before the key is used, so a crash cannot leave
// encrypted data behind whose key was never persisted. If the file does
// not exist and create is false, the returned error wraps fs.ErrNotExist.
//
// A key file readable by group or others is accepted with a warning. The
// key itself is never logged.
func LoadOrCreateKeyFile(path string, create bool, log *slog.Logger) (key []byte, created bool, err error) {
	key, err = readKeyFile(path, log)
	if err == nil || !errors.Is(err, fs.ErrNotExist) || !create {
		return key, false, err
	}
	key, err = createKeyFile(path)
	if errors.Is(err, fs.ErrExist) {
		// Created by someone else since the read above: use that key.
		key, err = readKeyFile(path, log)
		return key, false, err
	}
	if err != nil {
		return nil, false, err
	}
	log.Info("created a new secret key; keep a copy of this file outside the host", "path", path)
	return key, true, nil
}

func readKeyFile(path string, log *slog.Logger) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // G304: path from configuration (data directory)
	if err != nil {
		return nil, fmt.Errorf("reading secret key file: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("reading secret key file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("secret key file %s is not a regular file", path)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		log.Warn("secret key file is readable by group or others; restrict it with chmod 600",
			"path", path, "mode", perm.String())
	}
	b, err := io.ReadAll(io.LimitReader(f, maxKeyFileSize))
	if err != nil {
		return nil, fmt.Errorf("reading secret key file: %w", err)
	}
	key, err := ParseKey(string(b))
	if err != nil {
		// ParseKey errors never contain the key.
		return nil, fmt.Errorf("secret key file %s: %w", path, err)
	}
	return key, nil
}

func createKeyFile(path string) ([]byte, error) {
	enc, err := GenerateKey()
	if err != nil {
		return nil, err
	}
	key, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return nil, fmt.Errorf("secrets: %w", err)
	}
	// O_EXCL: fail instead of overwriting a key that appeared meanwhile.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: path from configuration
	if err != nil {
		return nil, fmt.Errorf("creating secret key file: %w", err)
	}
	_, werr := f.WriteString(enc + "\n")
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		_ = os.Remove(path) // created above by us, holds no usable key
		return nil, fmt.Errorf("writing secret key file: %w", werr)
	}
	// Sync the directory so the new directory entry is durable too.
	if err := syncDir(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("syncing secret key directory: %w", err)
	}
	return key, nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // G304: directory of the key file
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return err
	}
	return d.Close()
}
