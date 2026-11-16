# Changelog

All notable changes to StackVault. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), versions follow
[Semantic Versioning](https://semver.org/).

## [Unreleased]

### Changed

- **The secret key can now live in the data directory.** If neither
  `STACKVAULT_SECRET_KEY` nor `STACKVAULT_SECRET_KEY_FILE` is set, StackVault
  uses `<STACKVAULT_DATA_DIR>/secret.key` and generates it on the first start
  of a new installation. A backup of the data directory then has everything
  it needs to run again: the database and the key that decrypts the stored
  repository passwords. This is simpler for most people, since there is no
  key to create and no separate file to keep outside the backups.

  **Security trade-off:** this is a small step back from 0.1. Anyone who gets
  a copy of the data directory now also has the key, for example from a
  stolen unencrypted copy, or from the StackVault stack's restic backup
  together with that repository's password. With the key, they can decrypt
  the stored repository passwords and backend credentials. If you prefer the
  0.1 behaviour, keep the key outside `STACKS_ROOT` and set
  `STACKVAULT_SECRET_KEY_FILE` (see "Optional: keep the key out of the
  backups" in the README).

  Existing installations are not changed. A key configured with
  `STACKVAULT_SECRET_KEY` or `STACKVAULT_SECRET_KEY_FILE` keeps working. To
  switch, move the key file to `data/secret.key` and remove the variable.
  Never let StackVault generate a new key for an existing database: it
  refuses to start in that case.

### Added

- **Scheduled updates**: a schedule on the Settings tab can now queue an
  update instead of a backup. Scheduled updates always run "only if new",
  so nothing is stopped or backed up unless an image changed. Options:
  snapshot before stopping, and roll back in place. Updates (manual and
  scheduled) can now skip the snapshot before stopping (worker flag
  `--no-pre-stop`).
- **Update tab** for users with the update right (the update moved off the
  Backups tab):
  - **Check now** asks the registries, without pulling, whether the tags of
    the stack's images point to newer images, and lists every service with
    its running and available version (the image's version label if it has
    one, otherwise build date and digest).
  - **Only if there is something new** (on by default): the update job
    pulls, and if no running service gets a new image it ends there,
    without backup and without restarting anything.
  - **Roll back in place**: a failed update restores its backup in place,
    rewriting only the files changed since the backup. The compose editor
    has the same option. Worker flags `--only-if-new`,
    `--rollback-in-place`.
  - **Unpin**: containers that a restore or rollback started with pinned
    image versions (by digest) are marked in the check, and "Unpin now"
    switches them back to the tags of the compose file (an update with
    backup; "only if new" counts pinned containers as changed).
- **In-place restore** for large stacks: the restore page offers a second
  method next to "into a new directory, then switch over". In place, restic
  restores over the stack directory: unchanged files (same size and
  modification time) are kept, changed files are rewritten, files that are
  not in the snapshot are deleted. This saves the download of the whole
  snapshot and the disk space of a second copy. There is no
  `<stack>.old-<time>` directory: the safety snapshot is the only way back,
  and if the restore fails half-way the stack is left stopped and partly
  restored until it is restored again. Worker flag `--in-place`. Automatic
  rollbacks of updates and compose changes still switch over.
- **Container shell** (opt-in, administrators only): with
  `STACKVAULT_ENABLE_SHELL=true` a "Shell" tab opens a terminal in the browser
  for any running container of a stack (`bash`, or `sh`, as the container's
  user). Off by default, because it is free command execution; when off the
  tab does not exist. Opening and closing a shell is audited, the commands are
  not.
- **Compose editor** (administrators only): a "Compose" tab to edit a stack's
  main compose file. Applying validates the file with `docker compose config`,
  backs the stack up, writes the file, starts the services and rolls back to
  the backup if they do not come up within 5 minutes. Changes made to the file
  on disk after it was opened in the editor are never overwritten. Without a
  repository it works after an explicit confirmation, but without backup and
  without rollback.
- **Update** of a stack (new per-stack permission "Update"): pulls the newest
  images of the running services while the stack keeps running, backs it up
  (snapshot while running, then stopped), recreates the services and waits
  up to 5 minutes until they run and are healthy. If they do not come up,
  the update is **rolled back automatically**: the snapshot taken just before
  is restored and the stack starts with its previous image versions. A stack
  without repository can be updated after an explicit confirmation, but then
  without backup and **without rollback**.
- **Logs tab**: live logs of all containers of a stack, mixed in the order
  `docker compose logs` prints them, with one checkbox per container to show
  or hide its lines, optional line wrapping (off = horizontal scrolling) and
  a full-window mode that shows only the log. Viewing logs is a new per-stack permission ("Logs" in
  the stack settings; admins always have it), because application logs are
  not redacted and may contain secrets. The manager runs `docker compose
  logs` itself, read-only, at most 4 open views per user and 20 in total.
- Key check at startup: the database stores a check value, and the manager
  refuses to start if the configured secret key does not match the database
  ("the secret key does not match this database"), instead of failing later
  when it decrypts a repository password. A 0.1 database gets the check value
  on the first start, after the key has decrypted one of its stored
  repository secrets.

### Fixed

- Stacks whose compose file is named `docker-compose.yml` or
  `docker-compose.yaml` are now discovered. StackVault accepts the names
  `docker compose` looks for, in its order: `compose.yaml`, `compose.yml`,
  `docker-compose.yml`, `docker-compose.yaml`. If a directory has both
  `compose.yml` and `compose.yaml`, `compose.yaml` is now used, as in
  `docker compose`.

## [0.1.0] - 2026-11-15

First release.

[Unreleased]: https://github.com/MB512/stackvault/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/MB512/stackvault/releases/tag/v0.1.0
