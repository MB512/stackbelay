-- Whether a stack's directory existed at some point (issue #27): a stack
-- without directory is hidden from the stack list only if it vanished, not
-- if it was imported from another host and is not restored yet. Existing
-- records count as seen, except imported stacks (source host set) that are
-- not present and never had a successful restore.
ALTER TABLE stacks ADD COLUMN seen_present INTEGER NOT NULL DEFAULT 1;
UPDATE stacks SET seen_present = 0
 WHERE present = 0 AND source_host <> ''
   AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.stack_id = stacks.id AND j.type = 'restore' AND j.state = 'succeeded');
