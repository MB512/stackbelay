-- Right to update a stack (pull new images, back up, recreate containers).
ALTER TABLE permissions ADD COLUMN can_update INTEGER NOT NULL DEFAULT 0;
