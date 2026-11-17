-- Right to stop, start and restart a stack without backup.
ALTER TABLE permissions ADD COLUMN can_operate INTEGER NOT NULL DEFAULT 0;
