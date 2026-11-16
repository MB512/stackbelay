-- Right to view the live container logs of a stack. Logs are not redacted
-- (applications may log secrets), so this is a grant of its own.
ALTER TABLE permissions ADD COLUMN can_logs INTEGER NOT NULL DEFAULT 0;
