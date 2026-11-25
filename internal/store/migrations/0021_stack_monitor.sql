-- Stack monitor (issue #49).
--
-- notify_events also carries stack events now: kind '' is a finished job
-- (job_id set), any other kind is a notification key of the monitor
-- (stack.degraded, ...) with stack_id (NULL for the whole host) and a
-- JSON detail. SQLite cannot drop NOT NULL, so the table is rebuilt.
CREATE TABLE notify_events_new (
  id         INTEGER PRIMARY KEY,
  created_at TEXT NOT NULL,
  job_id     TEXT REFERENCES jobs(id) ON DELETE CASCADE,
  stack_id   INTEGER REFERENCES stacks(id) ON DELETE CASCADE,
  kind       TEXT NOT NULL DEFAULT '',
  detail     TEXT NOT NULL DEFAULT '{}',
  CHECK ((job_id IS NOT NULL) = (kind = ''))
) STRICT;
INSERT INTO notify_events_new (id, created_at, job_id) SELECT id, created_at, job_id FROM notify_events;
DROP TABLE notify_events;
ALTER TABLE notify_events_new RENAME TO notify_events;

-- The monitor's view of each stack: the confirmed state, a different
-- state seen in the last polls (debounce), whether an alert went out for
-- the current bad state, and the newest finished job it has seen.
CREATE TABLE stack_health (
  stack_id        INTEGER PRIMARY KEY REFERENCES stacks(id) ON DELETE CASCADE,
  state           TEXT NOT NULL,             -- started, degraded, stopped, unknown, missing
  since           TEXT NOT NULL,
  alerted         INTEGER NOT NULL DEFAULT 0,
  candidate       TEXT NOT NULL DEFAULT '',
  candidate_since TEXT NOT NULL DEFAULT '',
  candidate_count INTEGER NOT NULL DEFAULT 0,
  last_job_id     TEXT NOT NULL DEFAULT ''
) STRICT;
