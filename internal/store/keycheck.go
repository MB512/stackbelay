package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrKeyMismatch is returned by Open when the secret key cannot decrypt
// the data in the database.
var ErrKeyMismatch = errors.New("the secret key does not match this database")

// Plaintext and associated data of the "key_check" row in meta. They are
// part of the stored ciphertext and contain no product name, so a rename
// of the project never touches them (issue #44).
var (
	keyCheckPlain = []byte("key check")
	keyCheckAAD   = []byte("key-check")
)

// checkKey verifies that the store's secrets box opens this database. A
// database without a key_check row (a new one) gets it written; if it
// already has a repository, that secret must decrypt with its own
// associated data first, so the row is never written for a wrong key.
func (s *Store) checkKey(ctx context.Context) error {
	var sealed []byte
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = 'key_check'`).Scan(&sealed)
	switch {
	case err == nil:
		pt, err := s.box.Open(sealed, keyCheckAAD)
		if err == nil && bytes.Equal(pt, keyCheckPlain) {
			return nil
		}
		return ErrKeyMismatch
	case !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("reading key check: %w", err)
	}

	var repoID int64
	var secret []byte
	err = s.db.QueryRowContext(ctx, `SELECT id, secret_enc FROM repositories ORDER BY id LIMIT 1`).
		Scan(&repoID, &secret)
	switch {
	case err == nil:
		if _, err := s.box.Open(secret, repoAAD(repoID)); err != nil {
			return ErrKeyMismatch
		}
	case !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("reading repository for key check: %w", err)
	}

	sealed, err = s.box.Seal(keyCheckPlain, keyCheckAAD)
	if err != nil {
		return err
	}
	// DO NOTHING: another process may have written the row meanwhile; the
	// next start checks it.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO meta (key, value) VALUES ('key_check', ?) ON CONFLICT (key) DO NOTHING`, sealed); err != nil {
		return fmt.Errorf("writing key check: %w", err)
	}
	return nil
}
