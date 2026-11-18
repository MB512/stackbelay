-- Sizes and durations (issue #9): the "summary" restic >= 0.17 records in
-- every snapshot (data added, bytes and files processed, start and end of
-- the backup), as JSON; NULL for older snapshots without one.
ALTER TABLE snapshots ADD COLUMN summary TEXT;
