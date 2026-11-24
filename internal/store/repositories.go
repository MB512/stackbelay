package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/MB512/stackbelay/internal/restic"
)

// Repositories are global (issue #39): admins manage them on their own
// tab, and each stack references at most one (stacks.repository_id).
// Several stacks may use the same repository; a change of it applies to
// all of them from their next job on.

// ErrInUse is returned by DeleteRepository while stacks use the repository.
var ErrInUse = errors.New("in use")

// MaxRepositoryNameLen is the maximum length of a repository name, in
// characters.
const MaxRepositoryNameLen = 200

// Repository is a restic repository with decrypted credentials. Never log
// a Repository value.
type Repository struct {
	ID       int64
	Name     string // unique, shown in the UI
	Location string // restic repository string; contains no credentials
	Password string
	// Env holds backend credentials (keys from restic.BackendEnvVars,
	// except the password variables).
	Env map[string]string
	// SnapshotsRefreshedAt is the time of the last snapshot cache refresh;
	// zero if never refreshed.
	SnapshotsRefreshedAt time.Time
}

// Restic returns the restic.Repo used to run restic against r.
func (r Repository) Restic() restic.Repo {
	keys := make([]string, 0, len(r.Env))
	for k := range r.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	env := []string{"RESTIC_PASSWORD=" + r.Password}
	for _, k := range keys {
		env = append(env, k+"="+r.Env[k])
	}
	return restic.Repo{Location: r.Location, Env: env}
}

// RepositoryInfo is the non-secret part of a repository, for display.
type RepositoryInfo struct {
	ID                   int64
	Name                 string
	Location             string
	SnapshotsRefreshedAt time.Time // zero if never refreshed
	// Stacks are the names of the stacks that use the repository, sorted
	// (filled by ListRepositories and RepositoryInfo only).
	Stacks []string
}

// repoSecret is the plaintext of repositories.secret_enc.
type repoSecret struct {
	Password string            `json:"password"`
	Env      map[string]string `json:"env,omitempty"`
}

// repoAAD binds a repository secret to its row, so a ciphertext copied to
// another row does not decrypt. It contains no product name, so a rename
// of the project never touches it (issue #44).
func repoAAD(repoID int64) []byte {
	return []byte("repository:id=" + strconv.FormatInt(repoID, 10))
}

// sealSecret encrypts the credentials of repository repoID.
func (s *Store) sealSecret(repoID int64, password string, env map[string]string) ([]byte, error) {
	// The JSON is encrypted right below and never stored or logged in plain text.
	plain, err := json.Marshal(repoSecret{Password: password, Env: env}) //nolint:gosec // G117: see above
	if err != nil {
		return nil, fmt.Errorf("encoding repository secret: %w", err)
	}
	return s.box.Seal(plain, repoAAD(repoID))
}

// openSecret decrypts the credentials of repository repoID.
func (s *Store) openSecret(repoID int64, enc []byte) (repoSecret, error) {
	plain, err := s.box.Open(enc, repoAAD(repoID))
	if err != nil {
		return repoSecret{}, fmt.Errorf("decrypting secret of repository %d: %w", repoID, err)
	}
	var sec repoSecret
	if err := json.Unmarshal(plain, &sec); err != nil {
		return repoSecret{}, fmt.Errorf("decoding secret of repository %d: %w", repoID, err)
	}
	return sec, nil
}

// ValidateRepositoryName checks a repository name. The error messages are
// meant for the user.
func ValidateRepositoryName(name string) error {
	switch {
	case name == "" || strings.TrimSpace(name) != name:
		return errors.New("repository name must not be empty or have surrounding spaces")
	case !utf8.ValidString(name) || strings.IndexFunc(name, unicode.IsControl) >= 0:
		return errors.New("repository name contains invalid characters")
	case utf8.RuneCountInString(name) > MaxRepositoryNameLen:
		return fmt.Errorf("repository name is longer than %d characters", MaxRepositoryNameLen)
	}
	return nil
}

// ValidateRepository checks a repository before it is saved; the create
// and update functions call it too. The error messages are meant for the
// user.
func ValidateRepository(location, password string, env map[string]string) error {
	switch {
	case location == "" || strings.TrimSpace(location) != location:
		return errors.New("repository location must not be empty or have surrounding spaces")
	case len(location) > 1024:
		return errors.New("repository location is too long")
	case strings.IndexFunc(location, unicode.IsControl) >= 0:
		return errors.New("repository location contains control characters")
	case strings.HasPrefix(location, "-"):
		return errors.New("repository location must not start with '-'")
	}
	// The location is stored unencrypted, so it must not carry a password,
	// e.g. "rest:https://user:pass@host/repo". Use RESTIC_REST_USERNAME /
	// RESTIC_REST_PASSWORD instead (restic.SplitRESTCredentials).
	if _, _, _, hasPw, err := restic.URLCredentials(location); err != nil {
		return fmt.Errorf("repository location: %w", err)
	} else if hasPw {
		return errors.New("repository location must not contain a password; use RESTIC_REST_USERNAME/RESTIC_REST_PASSWORD")
	}
	if password == "" || strings.ContainsRune(password, 0) {
		return errors.New("repository password must not be empty")
	}
	for k, v := range env {
		if !restic.ValidEnvKey(k) || k == "RESTIC_PASSWORD" || k == "RESTIC_PASSWORD_FILE" {
			return fmt.Errorf("environment variable %q is not allowed for repositories", k)
		}
		if strings.ContainsRune(v, 0) {
			return fmt.Errorf("environment variable %q contains a NUL byte", k)
		}
	}
	return nil
}

// isUniqueViolation reports whether err is a violated UNIQUE constraint.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// CreateRepository adds a repository and returns its id. A name that is
// already used gives ErrConflict.
func (s *Store) CreateRepository(ctx context.Context, name, location, password string, env map[string]string) (int64, error) {
	if err := ValidateRepositoryName(name); err != nil {
		return 0, err
	}
	if err := ValidateRepository(location, password, env); err != nil {
		return 0, err
	}
	var id int64
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		// The secret is bound to the id, which exists only after the
		// insert: insert with an empty secret, then seal and store it.
		err := tx.QueryRowContext(ctx,
			`INSERT INTO repositories (name, location, secret_enc) VALUES (?, ?, X'') RETURNING id`,
			name, location).Scan(&id)
		if isUniqueViolation(err) {
			return fmt.Errorf("repository %q: %w", name, ErrConflict)
		}
		if err != nil {
			return err
		}
		enc, err := s.sealSecret(id, password, env)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE repositories SET secret_enc = ? WHERE id = ?`, enc, id)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("creating repository: %w", err)
	}
	return id, nil
}

// UpdateRepository replaces name, location and credentials of a
// repository. If the location changes, the snapshot cache of the old
// location is dropped. A name that another repository uses gives
// ErrConflict.
func (s *Store) UpdateRepository(ctx context.Context, id int64, name, location, password string, env map[string]string) error {
	if err := ValidateRepositoryName(name); err != nil {
		return err
	}
	if err := ValidateRepository(location, password, env); err != nil {
		return err
	}
	enc, err := s.sealSecret(id, password, env)
	if err != nil {
		return err
	}
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		var oldLocation string
		err := tx.QueryRowContext(ctx, `SELECT location FROM repositories WHERE id = ?`, id).Scan(&oldLocation)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("repository %d: %w", id, ErrNotFound)
		}
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE repositories SET name = ?, location = ?, secret_enc = ? WHERE id = ?`,
			name, location, enc, id)
		if isUniqueViolation(err) {
			return fmt.Errorf("repository %q: %w", name, ErrConflict)
		}
		if err != nil {
			return err
		}
		if oldLocation != location {
			if _, err := tx.ExecContext(ctx, `DELETE FROM snapshots WHERE repo_id = ?`, id); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `UPDATE repositories SET snapshots_refreshed_at = NULL WHERE id = ?`, id)
			return err
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("saving repository %d: %w", id, err)
	}
	return nil
}

// DeleteRepository removes a repository and its snapshot cache. It refuses
// with ErrInUse while a stack uses it.
func (s *Store) DeleteRepository(ctx context.Context, id int64) error {
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM stacks WHERE repository_id = ?`, id).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return fmt.Errorf("repository %d is used by %d stacks: %w", id, n, ErrInUse)
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM repositories WHERE id = ?`, id)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return fmt.Errorf("repository %d: %w", id, ErrNotFound)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("deleting repository: %w", err)
	}
	return nil
}

// SetStackRepository makes a stack use a repository; repoID 0 removes the
// stack's repository. An unknown stack or repository gives ErrNotFound.
func (s *Store) SetStackRepository(ctx context.Context, stackID, repoID int64) error {
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var ref any // NULL for "none"
		if repoID != 0 {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM repositories WHERE id = ?`, repoID).Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				return fmt.Errorf("repository %d: %w", repoID, ErrNotFound)
			}
			ref = repoID
		}
		res, err := tx.ExecContext(ctx, `UPDATE stacks SET repository_id = ? WHERE id = ?`, ref, stackID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			return fmt.Errorf("stack %d: %w", stackID, ErrNotFound)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("setting repository of stack %d: %w", stackID, err)
	}
	return nil
}

// repoColumns are the columns read by scanRepo, from repositories r.
const repoColumns = `r.id, r.name, r.location, r.secret_enc, r.snapshots_refreshed_at`

// scanRepo reads repoColumns and decrypts the secret unless decrypt is
// false.
func (s *Store) scanRepo(row interface{ Scan(...any) error }, decrypt bool) (Repository, error) {
	var r Repository
	var enc []byte
	var refreshed sql.NullString
	if err := row.Scan(&r.ID, &r.Name, &r.Location, &enc, &refreshed); err != nil {
		return Repository{}, err
	}
	if refreshed.Valid {
		t, err := parseTime(refreshed.String)
		if err != nil {
			return Repository{}, err
		}
		r.SnapshotsRefreshedAt = t
	}
	if decrypt {
		sec, err := s.openSecret(r.ID, enc)
		if err != nil {
			return Repository{}, err
		}
		r.Password, r.Env = sec.Password, sec.Env
	}
	return r, nil
}

func (r Repository) info() RepositoryInfo {
	return RepositoryInfo{ID: r.ID, Name: r.Name, Location: r.Location, SnapshotsRefreshedAt: r.SnapshotsRefreshedAt}
}

// Repository loads and decrypts a repository, or returns ErrNotFound.
func (s *Store) Repository(ctx context.Context, id int64) (Repository, error) {
	r, err := s.scanRepo(s.db.QueryRowContext(ctx, `SELECT `+repoColumns+` FROM repositories r WHERE r.id = ?`, id), true)
	if errors.Is(err, sql.ErrNoRows) {
		return Repository{}, fmt.Errorf("repository %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return Repository{}, fmt.Errorf("loading repository %d: %w", id, err)
	}
	return r, nil
}

// RepositoryInfo loads a repository without decrypting its credentials,
// with the stacks that use it, or returns ErrNotFound.
func (s *Store) RepositoryInfo(ctx context.Context, id int64) (RepositoryInfo, error) {
	r, err := s.scanRepo(s.db.QueryRowContext(ctx, `SELECT `+repoColumns+` FROM repositories r WHERE r.id = ?`, id), false)
	if errors.Is(err, sql.ErrNoRows) {
		return RepositoryInfo{}, fmt.Errorf("repository %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return RepositoryInfo{}, fmt.Errorf("loading repository %d: %w", id, err)
	}
	ri := r.info()
	users, err := s.repositoryStacks(ctx)
	if err != nil {
		return RepositoryInfo{}, err
	}
	ri.Stacks = users[id]
	return ri, nil
}

// ListRepositories returns all repositories without credentials, with the
// stacks that use them, ordered by name.
func (s *Store) ListRepositories(ctx context.Context) ([]RepositoryInfo, error) {
	users, err := s.repositoryStacks(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+repoColumns+` FROM repositories r ORDER BY r.name, r.id`)
	if err != nil {
		return nil, fmt.Errorf("listing repositories: %w", err)
	}
	defer rows.Close()
	var out []RepositoryInfo
	for rows.Next() {
		r, err := s.scanRepo(rows, false)
		if err != nil {
			return nil, fmt.Errorf("listing repositories: %w", err)
		}
		ri := r.info()
		ri.Stacks = users[r.ID]
		out = append(out, ri)
	}
	return out, rows.Err()
}

// repositoryStacks returns the names of the stacks per repository id,
// sorted.
func (s *Store) repositoryStacks(ctx context.Context) (map[int64][]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT repository_id, name FROM stacks WHERE repository_id IS NOT NULL ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("listing repository users: %w", err)
	}
	defer rows.Close()
	out := map[int64][]string{}
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, fmt.Errorf("listing repository users: %w", err)
		}
		out[id] = append(out[id], name)
	}
	return out, rows.Err()
}

// stackRepo loads the repository of a stack.
func (s *Store) stackRepo(ctx context.Context, stackID int64, decrypt bool) (Repository, error) {
	r, err := s.scanRepo(s.db.QueryRowContext(ctx,
		`SELECT `+repoColumns+` FROM stacks st JOIN repositories r ON r.id = st.repository_id WHERE st.id = ?`,
		stackID), decrypt)
	if errors.Is(err, sql.ErrNoRows) {
		return Repository{}, fmt.Errorf("repository for stack %d: %w", stackID, ErrNotFound)
	}
	if err != nil {
		return Repository{}, fmt.Errorf("loading repository for stack %d: %w", stackID, err)
	}
	return r, nil
}

// RepositoryInfoForStack loads the repository a stack uses without
// decrypting its credentials (Stacks is not filled), or returns
// ErrNotFound if the stack has none.
func (s *Store) RepositoryInfoForStack(ctx context.Context, stackID int64) (RepositoryInfo, error) {
	r, err := s.stackRepo(ctx, stackID, false)
	if err != nil {
		return RepositoryInfo{}, err
	}
	return r.info(), nil
}

// RepositoryForStack loads and decrypts the repository a stack uses, or
// returns ErrNotFound if the stack has none.
func (s *Store) RepositoryForStack(ctx context.Context, stackID int64) (Repository, error) {
	return s.stackRepo(ctx, stackID, true)
}
