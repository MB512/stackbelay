-- Notification summary (issue #48): when it is sent (cron expression, in
-- the manager's TZ), after how many days without a consistent backup a
-- stack counts as stale, and when the last summary was sent (the start of
-- the next summary's period; NULL until the first summary is due).
ALTER TABLE notify_settings ADD COLUMN summary_cron TEXT NOT NULL DEFAULT '0 7 * * *';
ALTER TABLE notify_settings ADD COLUMN stale_days INTEGER NOT NULL DEFAULT 2;
ALTER TABLE notify_settings ADD COLUMN summary_sent_at TEXT;
