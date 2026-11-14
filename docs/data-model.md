# Data model (SQLite)

Stacks are discovered from the filesystem (see architecture.md) and mirrored
into the DB so permissions, repositories and schedules can reference them.

```sql
CREATE TABLE users (
  id            INTEGER PRIMARY KEY,
  username      TEXT NOT NULL UNIQUE,
  password_hash TEXT NOT NULL,              -- argon2id
  is_admin      INTEGER NOT NULL DEFAULT 0, -- admins manage users/stacks/repos
  disabled      INTEGER NOT NULL DEFAULT 0,
  created_at    TEXT NOT NULL,
  must_change_password INTEGER NOT NULL DEFAULT 0 -- generated password (initial admin); migration 0002
);

CREATE TABLE sessions (
  id          TEXT PRIMARY KEY,             -- SHA-256 (hex) of the random 256-bit cookie token
  user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  csrf_token  TEXT NOT NULL,
  expires_at  TEXT NOT NULL
);

CREATE TABLE stacks (
  id          INTEGER PRIMARY KEY,
  name        TEXT NOT NULL UNIQUE,         -- directory name under STACKS_ROOT
  enabled     INTEGER NOT NULL DEFAULT 1,
  present     INTEGER NOT NULL DEFAULT 1    -- 0 = directory missing (vanished, or imported stack not restored yet)
);

CREATE TABLE repositories (
  id            INTEGER PRIMARY KEY,
  stack_id      INTEGER NOT NULL UNIQUE REFERENCES stacks(id) ON DELETE CASCADE,
  location      TEXT NOT NULL,              -- restic repo string (path, s3:..., sftp:...)
  secret_enc    BLOB NOT NULL,              -- AES-GCM: password + backend env vars
  snapshots_refreshed_at TEXT
);

CREATE TABLE snapshots (
  repo_id      INTEGER NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
  snapshot_id  TEXT NOT NULL,               -- full restic id
  short_id     TEXT NOT NULL,
  time         TEXT NOT NULL,               -- snapshot time (UTC)
  hostname     TEXT NOT NULL,               -- source host, relevant for migration
  paths        TEXT NOT NULL,               -- JSON array, paths[0] = stack dir on source
  tags         TEXT NOT NULL DEFAULT '[]',  -- JSON array (pre-stop, final, ...)
  missing      INTEGER NOT NULL DEFAULT 0,  -- 1 = no longer in repo at last refresh
  PRIMARY KEY (repo_id, snapshot_id)
);

CREATE TABLE permissions (
  user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  stack_id    INTEGER NOT NULL REFERENCES stacks(id) ON DELETE CASCADE,
  can_backup  INTEGER NOT NULL DEFAULT 0,
  can_restore INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (user_id, stack_id)
);

CREATE TABLE schedules (
  id          INTEGER PRIMARY KEY,
  stack_id    INTEGER NOT NULL REFERENCES stacks(id) ON DELETE CASCADE,
  cron_expr   TEXT NOT NULL,
  pre_stop    INTEGER NOT NULL DEFAULT 0,   -- take pre-stop snapshot first
  enabled     INTEGER NOT NULL DEFAULT 1
);

CREATE TABLE jobs (
  id            TEXT PRIMARY KEY,           -- ULID
  stack_id      INTEGER NOT NULL REFERENCES stacks(id),
  type          TEXT NOT NULL,              -- backup | restore
  trigger       TEXT NOT NULL,              -- manual | schedule
  requested_by  INTEGER REFERENCES users(id), -- NULL for schedule
  state         TEXT NOT NULL,              -- queued | running | succeeded | failed | cancelled
  options       TEXT NOT NULL DEFAULT '{}', -- JSON (pre_stop, snapshot id, ...)
  snapshot_id   TEXT,
  error         TEXT,
  queued_at     TEXT NOT NULL,
  started_at    TEXT,
  finished_at   TEXT
);
CREATE INDEX jobs_stack_state ON jobs(stack_id, state);

CREATE TABLE audit_log (
  id         INTEGER PRIMARY KEY,
  at         TEXT NOT NULL,
  user_id    INTEGER REFERENCES users(id),
  action     TEXT NOT NULL,                 -- login, job.create, repo.update, ...
  stack_id   INTEGER REFERENCES stacks(id),
  detail     TEXT                           -- never contains secrets
);
```

## Authorization rules

- Admin: may manage users, stacks, repositories, schedules, and may do
  everything on every stack.
- Non-admin: may start a backup only if `can_backup`, a restore only if
  `can_restore` for that stack. Viewing a stack's status/snapshots requires at
  least one of the two.
- Checks happen server-side on every request and again when the job is created.
  The worker never decides permissions.

## Notes

- Timestamps: UTC, RFC 3339 strings.
- Secrets key: `STACKVAULT_SECRET_KEY` (32 bytes, base64) from env or file. Losing
  it means losing access to stored repo credentials; document in README.
- Migrations: numbered SQL files (`internal/store/migrations/NNNN_name.sql`)
  embedded via `embed`, applied at startup, each in its own transaction, and
  recorded in `schema_migrations`. Never edit an applied migration; add a new
  one. A database with a newer schema than the binary is refused.
- `repositories.secret_enc`: AES-256-GCM (`internal/secrets`) of the JSON
  `{"password": "...", "env": {"RESTIC_REST_USERNAME": "...", ...}}`, with
  associated data `stackvault:repository:stack=<stack_id>`. Env keys must be
  in `restic.BackendEnvVars`. `location` is stored in plain text and must not
  contain a password (`rest:https://user:pw@...` is refused).
- Changing a repository's `location` deletes its cached snapshots.
- Snapshot cache refresh marks snapshots no longer in the repository as
  `missing = 1` instead of deleting them.
- All tables are `STRICT` (SQLite enforces column types). Connections enable
  `foreign_keys`, WAL mode, a busy timeout and `BEGIN IMMEDIATE` transactions.
- Job logs live in files (`/data/jobs/<id>/log.txt`), not in the DB.
