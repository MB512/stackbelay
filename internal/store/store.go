// Package store is the SQLite data access layer. The schema lives in
// numbered SQL migrations embedded into the binary and applied at startup.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/MB512/stackvault/internal/secrets"

	// Registers the pure-Go "sqlite" driver with database/sql. A blank
	// import runs only the package's registration, similar to referencing an
	// ADO.NET provider without using its types directly.
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// timeFormat is used for all timestamps (UTC, RFC 3339 with seconds).
const timeFormat = time.RFC3339

// Store wraps the database handle. Its methods are safe for concurrent use.
type Store struct {
	db  *sql.DB
	box *secrets.Box // encrypts repository credentials
}

// Open opens (creating if necessary) the SQLite database at path and applies
// pending migrations. box encrypts and decrypts stored credentials; if its
// key does not match the database, Open fails with ErrKeyMismatch.
func Open(ctx context.Context, dbPath string, box *secrets.Box) (*Store, error) {
	if box == nil {
		return nil, errors.New("store: secrets box is required")
	}
	if dbPath == "" || strings.ContainsAny(dbPath, "?#") {
		return nil, fmt.Errorf("invalid database path %q", dbPath)
	}
	// Create the file ourselves so it is only readable by its owner: it
	// holds password hashes and encrypted credentials. SQLite would use the
	// umask (typically 0644), and its -wal/-shm files copy the mode of the
	// database file.
	f, err := os.OpenFile(dbPath, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // G304: path from configuration
	if err != nil {
		return nil, fmt.Errorf("creating database file: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, err
	}

	// Per-connection settings: enforce foreign keys, WAL for concurrent
	// readers, wait instead of failing when the database is busy, and start
	// transactions as writers (BEGIN IMMEDIATE) to avoid lock upgrades that
	// fail with SQLITE_BUSY.
	dsn := dbPath + "?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening database: %w", err)
	}
	s := &Store{db: db, box: box}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := s.checkKey(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Ping checks that the database is reachable (used by the health check).
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

type migration struct {
	version int
	name    string
	sql     string
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, err
	}
	var ms []migration
	seen := map[int]bool{}
	for _, e := range entries {
		prefix, _, ok := strings.Cut(e.Name(), "_")
		v, err := strconv.Atoi(prefix)
		if !ok || err != nil || v <= 0 || seen[v] {
			return nil, fmt.Errorf("invalid migration file name %q", e.Name())
		}
		seen[v] = true
		b, err := fs.ReadFile(migrationFS, path.Join("migrations", e.Name()))
		if err != nil {
			return nil, err
		}
		ms = append(ms, migration{version: v, name: e.Name(), sql: string(b)})
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].version < ms[j].version })
	return ms, nil
}

// migrate applies all migrations that are not recorded in
// schema_migrations, each in its own transaction.
func (s *Store) migrate(ctx context.Context) error {
	ms, err := loadMigrations()
	if err != nil {
		return fmt.Errorf("loading migrations: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		name       TEXT NOT NULL,
		applied_at TEXT NOT NULL
	) STRICT`); err != nil {
		return fmt.Errorf("creating schema_migrations: %w", err)
	}

	var current int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
		return fmt.Errorf("reading schema version: %w", err)
	}
	if latest := ms[len(ms)-1].version; current > latest {
		return fmt.Errorf("database schema version %d is newer than this binary supports (%d)", current, latest)
	}

	for _, m := range ms {
		if m.version <= current {
			continue
		}
		err := s.inTx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, m.sql); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx,
				`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
				m.version, m.name, formatTime(time.Now()))
			return err
		})
		if err != nil {
			return fmt.Errorf("applying migration %s: %w", m.name, err)
		}
	}
	return nil
}

// inTx runs fn in a transaction and commits if fn returns nil. If fn fails
// (or panics), the transaction is rolled back, like a C# "using" block around
// a TransactionScope without Complete().
func (s *Store) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	// Rollback after a successful Commit is a no-op returning ErrTxDone.
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing transaction: %w", err)
	}
	return nil
}

// ErrNotFound is returned when a requested row does not exist.
var ErrNotFound = errors.New("not found")

func formatTime(t time.Time) string { return t.UTC().Format(timeFormat) }

func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(timeFormat, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parsing stored time %q: %w", s, err)
	}
	return t, nil
}

// isForeignKeyError reports whether err is a violated foreign key, i.e. a
// referenced row (user, stack) does not exist.
func isForeignKeyError(err error) bool {
	return strings.Contains(err.Error(), "FOREIGN KEY constraint failed")
}
