-- New content of the compose file for "compose-apply" jobs (compose
-- editor), kept apart from jobs so job lists stay small. Kept after the job
-- as the record of what was applied.
CREATE TABLE job_compose (
  job_id  TEXT PRIMARY KEY REFERENCES jobs(id) ON DELETE CASCADE,
  content BLOB NOT NULL
) STRICT;
