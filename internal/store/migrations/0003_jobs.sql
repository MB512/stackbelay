-- Job results besides the main snapshot (pre-stop/safety snapshot, previous
-- directory of a restore), as JSON.
ALTER TABLE jobs ADD COLUMN result TEXT NOT NULL DEFAULT '{}';

-- The per-stack lock: at most one running job per stack, enforced by the
-- database itself.
CREATE UNIQUE INDEX jobs_one_running_per_stack ON jobs(stack_id) WHERE state = 'running';
CREATE INDEX jobs_state_queued ON jobs(state, queued_at);
