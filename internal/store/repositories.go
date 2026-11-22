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

	"github.com/MB512/stackbelay/internal/restic"
)

// Repository is the restic repository of one stack, with decrypted
// credentials. Never log a Repository value.
type Repository struct {
	ID       int64
	StackID  int64
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

// repoSecret is the plaintext of repositories.secret_enc.
type repoSecret struct {
	Password string            `json:"password"`
	Env      map[string]string `json:"env,omitempty"`
}

// repoAAD binds a repository secret to its stack, so a ciphertext copied
// to another row does not decrypt. It keeps the old name StackVault on
// purpose (issue #44): changing it would make every stored credential
// unreadable.
func repoAAD(stackID int64) []byte {
	return []byte("stackvault:repository:stack=" + strconv.FormatInt(stackID, 10))
}

// SetRepository creates or replaces the repository of a stack. If the
// location changes, the snapshot cache of the old location is dropped.
func (s *Store) SetRepository(ctx context.Context, stackID int64, location, password string, env map[string]string) (int64, error) {
	if err := ValidateRepository(location, password, env); err != nil {
		return 0, err
	}
	// The JSON is encrypted right below and never stored or logged in plain text.
	plain, err := json.Marshal(repoSecret{Password: password, Env: env}) //nolint:gosec // G117: see above
	if err != nil {
		return 0, fmt.Errorf("encoding repository secret: %w", err)
	}
	enc, err := s.box.Seal(plain, repoAAD(stackID))
	if err != nil {
		return 0, err
	}

	var id int64
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM stacks WHERE id = ?`, stackID).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return fmt.Errorf("stack %d: %w", stackID, ErrNotFound)
		}

		var oldLocation string
		err := tx.QueryRowContext(ctx, `SELECT id, location FROM repositories WHERE stack_id = ?`, stackID).Scan(&id, &oldLocation)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return tx.QueryRowContext(ctx,
				`INSERT INTO repositories (stack_id, location, secret_enc) VALUES (?, ?, ?) RETURNING id`,
				stackID, location, enc).Scan(&id)
		case err != nil:
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE repositories SET location = ?, secret_enc = ? WHERE id = ?`,
			location, enc, id); err != nil {
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
		return 0, fmt.Errorf("saving repository for stack %d: %w", stackID, err)
	}
	return id, nil
}

// RepositoryInfo is the non-secret part of a repository, for display.
type RepositoryInfo struct {
	ID                   int64
	StackID              int64
	Location             string
	SnapshotsRefreshedAt time.Time // zero if never refreshed
}

// RepositoryInfoForStack loads a stack's repository without decrypting its
// credentials, or returns ErrNotFound.
func (s *Store) RepositoryInfoForStack(ctx context.Context, stackID int64) (RepositoryInfo, error) {
	ri := RepositoryInfo{StackID: stackID}
	var refreshed sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT id, location, snapshots_refreshed_at FROM repositories WHERE stack_id = ?`, stackID,
	).Scan(&ri.ID, &ri.Location, &refreshed)
	if errors.Is(err, sql.ErrNoRows) {
		return RepositoryInfo{}, fmt.Errorf("repository for stack %d: %w", stackID, ErrNotFound)
	}
	if err != nil {
		return RepositoryInfo{}, fmt.Errorf("loading repository for stack %d: %w", stackID, err)
	}
	if refreshed.Valid {
		if ri.SnapshotsRefreshedAt, err = parseTime(refreshed.String); err != nil {
			return RepositoryInfo{}, err
		}
	}
	return ri, nil
}

// RepositoryForStack loads and decrypts the repository of a stack.
func (s *Store) RepositoryForStack(ctx context.Context, stackID int64) (Repository, error) {
	r := Repository{StackID: stackID}
	var enc []byte
	var refreshed sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT id, location, secret_enc, snapshots_refreshed_at FROM repositories WHERE stack_id = ?`, stackID,
	).Scan(&r.ID, &r.Location, &enc, &refreshed)
	if errors.Is(err, sql.ErrNoRows) {
		return Repository{}, fmt.Errorf("repository for stack %d: %w", stackID, ErrNotFound)
	}
	if err != nil {
		return Repository{}, fmt.Errorf("loading repository for stack %d: %w", stackID, err)
	}
	if refreshed.Valid {
		if r.SnapshotsRefreshedAt, err = parseTime(refreshed.String); err != nil {
			return Repository{}, err
		}
	}
	plain, err := s.box.Open(enc, repoAAD(stackID))
	if err != nil {
		return Repository{}, fmt.Errorf("decrypting repository secret for stack %d: %w", stackID, err)
	}
	var sec repoSecret
	if err := json.Unmarshal(plain, &sec); err != nil {
		return Repository{}, fmt.Errorf("decoding repository secret for stack %d: %w", stackID, err)
	}
	r.Password, r.Env = sec.Password, sec.Env
	return r, nil
}

// ValidateRepository checks a repository before it is saved; SetRepository
// calls it too. The error messages are meant for the user.
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
