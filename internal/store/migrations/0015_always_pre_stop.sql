-- Every backup takes a pre-stop snapshot (issue #34): the per-schedule
-- option is gone.
ALTER TABLE schedules DROP COLUMN pre_stop;
