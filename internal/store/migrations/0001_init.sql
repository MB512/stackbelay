-- Initial schema, see docs/data-model.md.
-- STRICT tables make SQLite enforce the declared column types.

CREATE TABLE users (
  id            INTEGER PRIMARY KEY,
  username      TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,              -- argon2id
  is_admin      INTEGER NOT NULL DEFAULT 0,
  disabled      INTEGER NOT NULL DEFAULT 0,
  created_at    TEXT NOT NULL
) STRICT;

CREATE TABLE sessions (
  id          TEXT PRIMARY KEY,             -- hash of the random 256-bit session id
  user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  csrf_token  TEXT NOT NULL,
  expires_at  TEXT NOT NULL
) STRICT;

CREATE TABLE stacks (
  id          INTEGER PRIMARY KEY,
  name        TEXT NOT NULL UNIQUE,         -- directory name under STACKS_ROOT
  enabled     INTEGER NOT NULL DEFAULT 1,
  present     INTEGER NOT NULL DEFAULT 1    -- 0 = directory missing
) STRICT;

CREATE TABLE repositories (
  id            INTEGER PRIMARY KEY,
  stack_id      INTEGER NOT NULL UNIQUE REFERENCES stacks(id) ON DELETE CASCADE,
  location      TEXT NOT NULL,              -- restic repo string
  secret_enc    BLOB NOT NULL,              -- AES-GCM: password + backend env vars
  snapshots_refreshed_at TEXT
) STRICT;

CREATE TABLE snapshots (
  repo_id      INTEGER NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
  snapshot_id  TEXT NOT NULL,
  short_id     TEXT NOT NULL,
  time         TEXT NOT NULL,
  hostname     TEXT NOT NULL,
  paths        TEXT NOT NULL,               -- JSON array
  tags         TEXT NOT NULL DEFAULT '[]',  -- JSON array
  missing      INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (repo_id, snapshot_id)
) STRICT;

CREATE TABLE permissions (
  user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  stack_id    INTEGER NOT NULL REFERENCES stacks(id) ON DELETE CASCADE,
  can_backup  INTEGER NOT NULL DEFAULT 0,
  can_restore INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (user_id, stack_id)
) STRICT;

CREATE TABLE schedules (
  id          INTEGER PRIMARY KEY,
  stack_id    INTEGER NOT NULL REFERENCES stacks(id) ON DELETE CASCADE,
  cron_expr   TEXT NOT NULL,
  pre_stop    INTEGER NOT NULL DEFAULT 0,
  enabled     INTEGER NOT NULL DEFAULT 1
) STRICT;

CREATE TABLE jobs (
  id            TEXT PRIMARY KEY,           -- ULID
  stack_id      INTEGER NOT NULL REFERENCES stacks(id),
  type          TEXT NOT NULL,              -- backup | restore
  trigger       TEXT NOT NULL,              -- manual | schedule
  requested_by  INTEGER REFERENCES users(id),
  state         TEXT NOT NULL,              -- queued | running | succeeded | failed | cancelled
  options       TEXT NOT NULL DEFAULT '{}',
  snapshot_id   TEXT,
  error         TEXT,
  queued_at     TEXT NOT NULL,
  started_at    TEXT,
  finished_at   TEXT
) STRICT;
CREATE INDEX jobs_stack_state ON jobs(stack_id, state);

CREATE TABLE audit_log (
  id         INTEGER PRIMARY KEY,
  at         TEXT NOT NULL,
  user_id    INTEGER REFERENCES users(id),
  action     TEXT NOT NULL,
  stack_id   INTEGER REFERENCES stacks(id),
  detail     TEXT                           -- never contains secrets
) STRICT;
