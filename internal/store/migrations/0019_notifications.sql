-- Notifications through ntfy (issue #47).
--
-- notify_settings has exactly one row. Notifications are off while topic
-- is empty. auth_enc holds the AES-GCM sealed credentials (access token or
-- user/password), NULL without auth. events maps event keys to on/off as
-- JSON; a key that is missing has its default (internal/notify), so new
-- event types need no migration.
CREATE TABLE notify_settings (
  id         INTEGER PRIMARY KEY CHECK (id = 1),
  server_url TEXT NOT NULL DEFAULT 'https://ntfy.sh',
  topic      TEXT NOT NULL DEFAULT '',
  auth_enc   BLOB,
  events     TEXT NOT NULL DEFAULT '{}'
) STRICT;
INSERT INTO notify_settings (id) VALUES (1);

-- The outbox: one row per finished job, written in the same transaction as
-- the job's result. The notifier sends and deletes the rows; it decides
-- there (settings, per-stack mute) whether a row becomes a message.
CREATE TABLE notify_events (
  id         INTEGER PRIMARY KEY,
  created_at TEXT NOT NULL,
  job_id     TEXT NOT NULL REFERENCES jobs(id) ON DELETE CASCADE
) STRICT;

-- Per-stack mute (e.g. test stacks): no event messages for the stack.
ALTER TABLE stacks ADD COLUMN notify_muted INTEGER NOT NULL DEFAULT 0;
