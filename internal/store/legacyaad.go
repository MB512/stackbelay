package store

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
)

// Conversion of the encrypted data of StackVault 0.4 and older (issue #44).
// Those versions sealed the key check and the repository secrets with
// associated data that contained the old product name. AES-GCM refuses to
// open a ciphertext with different associated data, just as with a wrong
// key, so the old values are tried after the new ones fail. Once the key
// check opens with the old values the key is proven right, and every
// secret is sealed again with the new associated data in one transaction.
//
// TODO(#44): remove this file and its two callers in checkKey once all
// installations have started a version that contains it.

var (
	legacyKeyCheckPlain = []byte("stackvault key check")
	legacyKeyCheckAAD   = []byte("stackvault:key-check")
)

func legacyRepoAAD(stackID int64) []byte {
	return []byte("stackvault:repository:stack=" + strconv.FormatInt(stackID, 10))
}

// isLegacyKeyCheck reports whether sealed is a key check row of 0.4 or
// older that opens with the store's key.
func (s *Store) isLegacyKeyCheck(sealed []byte) bool {
	pt, err := s.box.Open(sealed, legacyKeyCheckAAD)
	return err == nil && bytes.Equal(pt, legacyKeyCheckPlain)
}

// reencryptLegacy seals the key check and every repository secret that
// opens only with the old associated data again with the new one. The
// caller has verified the key. A secret that opens with neither is left
// unchanged: it was unreadable before and stays so, with the same error
// when it is used.
func (s *Store) reencryptLegacy(ctx context.Context) error {
	// A transaction is like a TransactionScope in .NET: either every row is
	// converted or none. Rollback after a successful Commit is a no-op, so
	// the deferred call only matters on an early return.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("converting encrypted data: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	type row struct {
		id, stackID int64
		enc         []byte
	}
	rows, err := tx.QueryContext(ctx, `SELECT id, stack_id, secret_enc FROM repositories`)
	if err != nil {
		return fmt.Errorf("reading repository secrets: %w", err)
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.stackID, &r.enc); err != nil {
			_ = rows.Close()
			return fmt.Errorf("reading repository secrets: %w", err)
		}
		all = append(all, r)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("reading repository secrets: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("reading repository secrets: %w", err)
	}

	for _, r := range all {
		if _, err := s.box.Open(r.enc, repoAAD(r.stackID)); err == nil {
			continue // already converted
		}
		plain, err := s.box.Open(r.enc, legacyRepoAAD(r.stackID))
		if err != nil {
			continue // unreadable before, see above
		}
		enc, err := s.box.Seal(plain, repoAAD(r.stackID))
		if err != nil {
			return fmt.Errorf("sealing repository secret for stack %d: %w", r.stackID, err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE repositories SET secret_enc = ? WHERE id = ?`, enc, r.id); err != nil {
			return fmt.Errorf("writing repository secret for stack %d: %w", r.stackID, err)
		}
	}

	sealed, err := s.box.Seal(keyCheckPlain, keyCheckAAD)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO meta (key, value) VALUES ('key_check', ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`, sealed); err != nil {
		return fmt.Errorf("writing key check: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("converting encrypted data: %w", err)
	}
	return nil
}
