package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Conversion of the per-stack repositories to global ones (issue #39).
// Migration 0018 keeps every repository with its id, links it to the stack
// that owned it and sets the meta row "convert_repositories".
// convertRepositories then, once, in one transaction:
//
//   - re-seals each secret from the old associated data
//     (repository:stack=<owner>) to repoAAD(id),
//   - merges repositories with the same location, password and backend
//     variables into the one with the lowest id (its stacks move over,
//     the other entries and their snapshot caches go),
//   - names each repository after its location, made unique with " (2)",
//     " (3)", ...
//
// A secret that opens with neither associated data keeps its placeholder
// name ("repository-<id>") and is not merged; its credentials have to be
// entered again on the Repositories tab.
//
// TODO(#39): remove once both installations have run it, before v0.1.0.

// ownerAAD is the associated data of a secret before migration 0018.
func ownerAAD(stackID int64) []byte {
	return []byte("repository:stack=" + strconv.FormatInt(stackID, 10))
}

// conversionPending reports whether migration 0018 left repositories to
// convert.
func (s *Store) conversionPending(ctx context.Context) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM meta WHERE key = 'convert_repositories'`).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("checking repository conversion: %w", err)
	}
	return n > 0, nil
}

// openOwnerSecret decrypts a secret sealed before migration 0018 with the
// associated data of its owner stack, the one stack linked to it.
func (s *Store) openOwnerSecret(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, repoID int64, enc []byte) (repoSecret, bool) {
	var owner int64
	if err := q.QueryRowContext(ctx, `SELECT id FROM stacks WHERE repository_id = ? ORDER BY id LIMIT 1`, repoID).
		Scan(&owner); err != nil {
		return repoSecret{}, false
	}
	plain, err := s.box.Open(enc, ownerAAD(owner))
	if err != nil {
		return repoSecret{}, false
	}
	var sec repoSecret
	if err := json.Unmarshal(plain, &sec); err != nil {
		return repoSecret{}, false
	}
	return sec, true
}

// convertRepositories runs the conversion described above if it is
// pending. checkKey has proven the key before.
func (s *Store) convertRepositories(ctx context.Context) error {
	pending, err := s.conversionPending(ctx)
	if err != nil || !pending {
		return err
	}
	type repo struct {
		id       int64
		location string
		enc      []byte
		sec      repoSecret
		ok       bool // secret decrypted
	}
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id, location, secret_enc FROM repositories ORDER BY id`)
		if err != nil {
			return err
		}
		var repos []repo
		for rows.Next() {
			var r repo
			if err := rows.Scan(&r.id, &r.location, &r.enc); err != nil {
				_ = rows.Close()
				return err
			}
			repos = append(repos, r)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		// Decrypted after the loop: the transaction's connection is busy
		// while rows is open.
		for i := range repos {
			repos[i].sec, repos[i].ok = s.openOwnerSecret(ctx, tx, repos[i].id, repos[i].enc)
		}

		// Merge identical repositories into the first of their kind.
		keep := map[string]int64{} // identity -> surviving id
		var survivors []repo
		for _, r := range repos {
			if !r.ok {
				continue
			}
			id := identity(r.location, r.sec)
			first, dup := keep[id]
			if !dup {
				keep[id] = r.id
				survivors = append(survivors, r)
				continue
			}
			if _, err := tx.ExecContext(ctx, `UPDATE stacks SET repository_id = ? WHERE repository_id = ?`, first, r.id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM repositories WHERE id = ?`, r.id); err != nil {
				return err
			}
		}

		// Name and re-seal the survivors. Placeholder names of the others
		// ("repository-<id>") stay reserved.
		used := map[string]bool{}
		for _, r := range repos {
			if !r.ok {
				used["repository-"+strconv.FormatInt(r.id, 10)] = true
			}
		}
		for _, r := range survivors {
			name := uniqueName(nameFromLocation(r.location), used)
			used[name] = true
			enc, err := s.sealSecret(r.id, r.sec.Password, r.sec.Env)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE repositories SET name = ?, secret_enc = ? WHERE id = ?`,
				name, enc, r.id); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM meta WHERE key = 'convert_repositories'`)
		return err
	})
	if err != nil {
		return fmt.Errorf("converting repositories: %w", err)
	}
	return nil
}

// identity is the merge key of a repository: location, password and the
// sorted backend variables.
func identity(location string, sec repoSecret) string {
	b, _ := json.Marshal(struct {
		L string
		P string
		E [][2]string
	}{location, sec.Password, sortedEnv(sec.Env)})
	return string(b)
}

func sortedEnv(env map[string]string) [][2]string {
	out := make([][2]string, 0, len(env))
	for _, k := range slices.Sorted(maps.Keys(env)) {
		out = append(out, [2]string{k, env[k]})
	}
	return out
}

// nameFromLocation derives a repository name from its location (which
// contains no credentials, see ValidateRepository), shortened to leave
// room for a " (n)" suffix.
func nameFromLocation(location string) string {
	name := strings.TrimSpace(location)
	if utf8.RuneCountInString(name) > MaxRepositoryNameLen-10 {
		name = string([]rune(name)[:MaxRepositoryNameLen-10])
	}
	if ValidateRepositoryName(name) != nil {
		return "repository"
	}
	return name
}

// uniqueName returns name, or name with " (2)", " (3)", ... if it is used.
func uniqueName(name string, used map[string]bool) string {
	if !used[name] {
		return name
	}
	for n := 2; ; n++ {
		if c := fmt.Sprintf("%s (%d)", name, n); !used[c] {
			return c
		}
	}
}
