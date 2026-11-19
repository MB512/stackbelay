-- The stack-level enabled flag is gone (issue #37): each schedule has its
-- own. The schedules of a disabled stack stay off.
UPDATE schedules SET enabled = 0 WHERE stack_id IN (SELECT id FROM stacks WHERE enabled = 0);
ALTER TABLE stacks DROP COLUMN enabled;
