-- Global repositories (issue #39): a repository is no longer owned by one
-- stack; stacks pick one with stacks.repository_id (NULL = none). SQLite
-- cannot drop a NOT NULL UNIQUE column, so the table is rebuilt.
--
-- Every existing repository keeps its id and is linked to the stack that
-- owned it. Its secret is still sealed with the old associated data
-- (repository:stack=<stack id>), and its name is a placeholder: the
-- "convert_repositories" meta row makes store.Open re-seal the secrets
-- with repository:id=<id>, merge identical repositories and name them
-- after their location (Go code: SQL cannot decrypt).
--
-- Dropping a table with foreign keys enabled deletes its rows first, which
-- would cascade into the snapshot cache. So the cache is saved and copied
-- back.

CREATE TABLE snapshots_saved AS SELECT * FROM snapshots;
DROP TABLE snapshots;

CREATE TABLE repositories_new (
  id            INTEGER PRIMARY KEY,
  name          TEXT NOT NULL UNIQUE,
  location      TEXT NOT NULL,
  secret_enc    BLOB NOT NULL,
  snapshots_refreshed_at TEXT
) STRICT;
INSERT INTO repositories_new (id, name, location, secret_enc, snapshots_refreshed_at)
  SELECT id, 'repository-' || id, location, secret_enc, snapshots_refreshed_at FROM repositories;

CREATE TABLE repository_owners AS SELECT id, stack_id FROM repositories;
DROP TABLE repositories;
ALTER TABLE repositories_new RENAME TO repositories;

-- Added after the rebuild: a reference to the old table would make its
-- DROP fail. No ON DELETE action: a repository in use cannot be deleted.
ALTER TABLE stacks ADD COLUMN repository_id INTEGER REFERENCES repositories(id);
UPDATE stacks SET repository_id = (SELECT o.id FROM repository_owners o WHERE o.stack_id = stacks.id);
DROP TABLE repository_owners;
CREATE INDEX stacks_repository ON stacks(repository_id);

CREATE TABLE snapshots (
  repo_id      INTEGER NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
  snapshot_id  TEXT NOT NULL,
  short_id     TEXT NOT NULL,
  time         TEXT NOT NULL,
  hostname     TEXT NOT NULL,
  paths        TEXT NOT NULL,
  tags         TEXT NOT NULL DEFAULT '[]',
  missing      INTEGER NOT NULL DEFAULT 0,
  summary      TEXT,
  PRIMARY KEY (repo_id, snapshot_id)
) STRICT;
INSERT INTO snapshots (repo_id, snapshot_id, short_id, time, hostname, paths, tags, missing, summary)
  SELECT repo_id, snapshot_id, short_id, time, hostname, paths, tags, missing, summary FROM snapshots_saved;
DROP TABLE snapshots_saved;

INSERT INTO meta (key, value) SELECT 'convert_repositories', X'01' WHERE EXISTS (SELECT 1 FROM repositories);
