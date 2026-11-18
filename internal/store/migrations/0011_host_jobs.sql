-- Host jobs (issue #19): jobs and schedules that belong to no stack
-- (stack_id NULL), only the job type "cleanup". SQLite cannot drop NOT NULL
-- from a column, so both tables are rebuilt.
--
-- Dropping a table with foreign keys enabled deletes its rows first, which
-- would cascade into job_compose. So job_compose is saved and rebuilt too.

CREATE TABLE job_compose_saved AS SELECT job_id, content FROM job_compose;
DROP TABLE job_compose;

CREATE TABLE jobs_new (
  id            TEXT PRIMARY KEY,           -- ULID
  stack_id      INTEGER REFERENCES stacks(id), -- NULL = a host job
  type          TEXT NOT NULL,
  trigger       TEXT NOT NULL,
  requested_by  INTEGER REFERENCES users(id),
  state         TEXT NOT NULL,
  options       TEXT NOT NULL DEFAULT '{}',
  snapshot_id   TEXT,
  error         TEXT,
  queued_at     TEXT NOT NULL,
  started_at    TEXT,
  finished_at   TEXT,
  result        TEXT NOT NULL DEFAULT '{}',
  CHECK ((stack_id IS NULL) = (type = 'cleanup'))
) STRICT;
INSERT INTO jobs_new (id, stack_id, type, trigger, requested_by, state, options, snapshot_id, error,
                      queued_at, started_at, finished_at, result)
  SELECT id, stack_id, type, trigger, requested_by, state, options, snapshot_id, error,
         queued_at, started_at, finished_at, result FROM jobs;
DROP TABLE jobs;
ALTER TABLE jobs_new RENAME TO jobs;
CREATE INDEX jobs_stack_state ON jobs(stack_id, state);
CREATE INDEX jobs_state_queued ON jobs(state, queued_at);
-- The per-stack lock: at most one running job per stack. NULLs are
-- distinct in a unique index, so host jobs need their own: at most one
-- running host job. That no stack job runs beside it is ClaimNextJob's
-- rule (claims are serialized by BEGIN IMMEDIATE).
CREATE UNIQUE INDEX jobs_one_running_per_stack ON jobs(stack_id) WHERE state = 'running';
CREATE UNIQUE INDEX jobs_one_running_host ON jobs(state) WHERE state = 'running' AND stack_id IS NULL;

CREATE TABLE job_compose (
  job_id  TEXT PRIMARY KEY REFERENCES jobs(id) ON DELETE CASCADE,
  content BLOB NOT NULL
) STRICT;
INSERT INTO job_compose (job_id, content) SELECT job_id, content FROM job_compose_saved;
DROP TABLE job_compose_saved;

CREATE TABLE schedules_new (
  id                INTEGER PRIMARY KEY,
  stack_id          INTEGER REFERENCES stacks(id) ON DELETE CASCADE, -- NULL = a host schedule
  cron_expr         TEXT NOT NULL,
  pre_stop          INTEGER NOT NULL DEFAULT 0,
  enabled           INTEGER NOT NULL DEFAULT 1,
  job_type          TEXT NOT NULL DEFAULT 'backup' CHECK (job_type IN ('backup', 'update', 'cleanup')),
  rollback_in_place INTEGER NOT NULL DEFAULT 0,
  no_backup         INTEGER NOT NULL DEFAULT 0,
  CHECK ((stack_id IS NULL) = (job_type = 'cleanup'))
) STRICT;
INSERT INTO schedules_new (id, stack_id, cron_expr, pre_stop, enabled, job_type, rollback_in_place, no_backup)
  SELECT id, stack_id, cron_expr, pre_stop, enabled, job_type, rollback_in_place, no_backup FROM schedules;
DROP TABLE schedules;
ALTER TABLE schedules_new RENAME TO schedules;
