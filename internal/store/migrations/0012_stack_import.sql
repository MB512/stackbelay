-- Restore from another host (issue #1): the host whose snapshots a stack
-- lists and restores; '' = this host (STACKVAULT_HOSTNAME). Backups always
-- use this host's name.
ALTER TABLE stacks ADD COLUMN source_host TEXT NOT NULL DEFAULT '';
