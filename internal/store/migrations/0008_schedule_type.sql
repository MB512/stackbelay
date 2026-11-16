-- Schedules can queue updates as well as backups. pre_stop applies to
-- both; rollback_in_place only to updates.
ALTER TABLE schedules ADD COLUMN job_type TEXT NOT NULL DEFAULT 'backup' CHECK (job_type IN ('backup', 'update'));
ALTER TABLE schedules ADD COLUMN rollback_in_place INTEGER NOT NULL DEFAULT 0;
