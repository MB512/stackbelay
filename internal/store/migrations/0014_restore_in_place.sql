-- Every restore and rollback runs in place (issue #33): the per-schedule
-- choice of an in-place rollback is gone.
ALTER TABLE schedules DROP COLUMN rollback_in_place;
