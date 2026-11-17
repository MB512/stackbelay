-- Scheduled update on a stack without repository: the admin confirmed when
-- creating the schedule that it runs without backup and without rollback.
ALTER TABLE schedules ADD COLUMN no_backup INTEGER NOT NULL DEFAULT 0;
