# Changelog

All notable changes to StackVault. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), versions follow
[Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- **Status tab**, the new home of a stack: its containers with state,
  health and image, the recent jobs and "Back up now". Containers and jobs
  refresh every 10 seconds; this refresh does not extend the login session.
  The update check moved there: its result is a column of the services
  table and stays visible to everyone who may view the stack until the
  next check.
- **Stop, start and restart** a stack from the Status tab, without backup,
  as worker jobs with a new per-stack right "Operate" (admins always).
  StackVault's own stack can be restarted but not stopped.
- Worker commands `worker stop|start|restart <stack>`.
- Job progress: the job page shows the current phase of a running job and,
  while restic backs up or restores, a progress bar with bytes, files,
  speed and remaining time. Phases of a rollback are marked "rollback:".
  While the stack stops or the job waits for healthy services, the services
  still pending are named. A queued job says what it waits for (a job of
  the same stack or a free slot) and how many jobs are ahead. Image pulls
  of an update show the downloaded bytes per image.
- Buttons of slow actions (saving or initializing a repository, compose
  validation, update check, snapshot refresh, …) show a spinner and a short
  message while the server works, and cannot be clicked twice. The Logs tab
  says when it is still waiting for the first lines.
- Logs tab: each container's lines have their own color (name and a bar
  at the left), with a matching swatch next to its checkbox.
- Scheduled updates on stacks without repository: creating the schedule
  asks for the same "no backup, no rollback" confirmation as a manual
  update (checkbox and dialog); such schedules then update without backup.
  The manual update without repository also asks in a dialog now.

### Fixed

- Logs tab: the history of several containers was shown container by
  container instead of in time order, and a reconnect could repeat lines.
- Scheduled updates on a stack without repository always failed ("the
  stack has no repository"), and the job page listed "no snapshot before
  stopping" for updates that make no snapshot at all.

### Changed

- All times in the UI (jobs, snapshots, schedules, job and container logs,
  image dates) are shown in the manager's time zone (`TZ`) instead of UTC.
  An unknown `TZ` is now a configuration error instead of silently UTC.
- The snapshot list moved to its own "Backups" tab
  (`/stacks/<name>/backups`); "Back up now" and the recent jobs moved to
  the Status tab.
- The Update tab is gone: "Update…" on the Status tab opens the update
  form (with the last check and "Unpin now") as its own page.
- The Shell tab is gone: with `STACKVAULT_ENABLE_SHELL`, admins open a
  shell from the container's row on the Status tab. Each row also links
  to the Logs tab with only that container selected.
- Stack list: the "Status" column is now "Scheduling" (it shows whether
  scheduled backups are enabled); the columns "Snapshots", "Last snapshot"
  and "You may" are gone. A new "Last backup" column shows the newest
  backup job with its state, linking to it; its tooltip gives the time of
  the last successful backup (issue #23).

## [0.2.0] - 2026-11-16

### Upgrading from 0.1: the secret key can move into the data directory

**New default location of the secret key: `<STACKVAULT_DATA_DIR>/secret.key`**
(`data/secret.key` in the StackVault stack). If neither
`STACKVAULT_SECRET_KEY` nor `STACKVAULT_SECRET_KEY_FILE` is set, StackVault
uses that file and generates it on the first start of a new installation.
The StackVault stack's own backup then contains everything it needs to run
again, database and key, so backing up and restoring StackVault itself
(also onto a new host) needs no separate key file anymore.

**Existing installations keep working unchanged**: a key configured with
`STACKVAULT_SECRET_KEY` or `STACKVAULT_SECRET_KEY_FILE` is still used. You
may want to move it into the data directory for the simpler backup and
restore: stop StackVault, copy the key file to `data/secret.key` (mode
0600), remove the variable and the key's volume line from `compose.yml`,
start it again and run a backup of the StackVault stack. Step by step:
[Upgrading from 0.1](https://github.com/MB512/stackvault#upgrading-from-01-move-the-secret-key-into-the-data-directory).
StackVault now checks the key against the database at startup and refuses
to start if it does not match, and it never generates a new key next to an
existing database.

**Security trade-off:** a small step back from 0.1. Anyone who gets a copy
of the data directory, for example a stolen unencrypted copy, or the
StackVault stack's restic backup together with that repository's password,
now also has the key and can decrypt the stored repository passwords and
backend credentials. To keep the 0.1 behaviour, change nothing (or, for
new installations, see "Optional: keep the key out of the backups" in the
README).

### Added

- **Updates** (new per-stack right "Update"), on a new **Update** tab:
  - Pulls the newest images of the running services while the stack keeps
    running, backs it up (optional snapshot while running, then stopped),
    recreates the services and waits up to 5 minutes until they run and
    are healthy.
  - If they do not come up, the update is **rolled back automatically**:
    the snapshot taken just before is restored and the stack starts with
    its previous image versions. The rollback switches over to a restored
    copy (the failed version is kept) or, with **roll back in place**,
    rewrites only the files changed since the backup.
  - **Check now** asks the registries, without pulling, whether the tags
    of the stack's images point to newer images, and lists every service
    with its running and available version (the image's version label if
    it has one, otherwise build date and digest).
  - **Only if there is something new** (on by default): after the pull,
    the job ends without backup and without restarting anything if no
    running service gets a new image.
  - **Unpin**: containers that a restore or rollback started with pinned
    image versions (by digest) are marked, and "Unpin now" switches them
    back to the tags of the compose file.
  - A stack without repository can be updated after an explicit
    confirmation, but then without backup and **without rollback**.
- **Scheduled updates**: a schedule on the Settings tab queues a backup or
  an update. Scheduled updates always run "only if new", so nothing is
  stopped or backed up unless an image changed. Options: snapshot before
  stopping, roll back in place.
- **In-place restore** for large stacks: next to "into a new directory,
  then switch over", restic can restore over the stack directory:
  unchanged files (same size and modification time) are kept, changed
  files are rewritten, files that are not in the snapshot are deleted.
  This saves the download of the whole snapshot and the disk space of a
  second copy. There is no `<stack>.old-<time>` directory: the safety
  snapshot is the only way back, and if the restore fails half-way the
  stack is left stopped and partly restored until it is restored again.
- **Logs tab** (new per-stack right "Logs", because application logs are
  not redacted): live logs of all containers of a stack, mixed in the
  order `docker compose logs` prints them, a checkbox per container,
  optional line wrapping (off = horizontal scrolling) and a full-window
  mode with the container filter. The manager runs `docker compose logs`
  itself, read-only, at most 4 open views per user and 20 in total.
- **Compose editor** (administrators only): a "Compose" tab to edit a
  stack's main compose file. Applying validates the file with `docker
  compose config`, backs the stack up, writes the file, starts the
  services and rolls back to the backup if they do not come up within 5
  minutes (optionally in place). Changes made to the file on disk after it
  was opened in the editor are never overwritten. Without a repository it
  works after an explicit confirmation, but without backup and rollback.
- **Container shell** (opt-in, administrators only): with
  `STACKVAULT_ENABLE_SHELL=true` a "Shell" tab opens a terminal in the
  browser (also full-window) for any running container of a stack (`bash`,
  or `sh`, as the container's user). Off by default, because it is free
  command execution; when off the tab does not exist. Opening and closing
  a shell is audited, the commands are not.
- Key check at startup: the manager refuses to start if the secret key
  does not match the database ("the secret key does not match this
  database"), instead of failing later when it decrypts a repository
  password. A 0.1 database gets the check value on the first start, after
  the key has decrypted one of its stored repository secrets.
- Worker flags: `restore --in-place`; `update [--no-backup] [--only-if-new]
  [--rollback-in-place] [--no-pre-stop]`.

### Changed

- The manager may now run, besides creating workers, read-only `docker
  compose logs/ps/config --quiet`, `docker image inspect` and `docker
  buildx imagetools inspect` (registry query, no pull) for discovered
  stacks, and `docker exec -it` for the opt-in shell; see
  `docs/security.md`. Everything that changes a stack still runs in a
  worker job.

### Fixed

- Stacks whose compose file is named `docker-compose.yml` or
  `docker-compose.yaml` are now discovered. StackVault accepts the names
  `docker compose` looks for, in its order: `compose.yaml`, `compose.yml`,
  `docker-compose.yml`, `docker-compose.yaml`. If a directory has both
  `compose.yml` and `compose.yaml`, `compose.yaml` is now used, as in
  `docker compose`.

## [0.1.0] - 2026-11-15

First release.

[Unreleased]: https://github.com/MB512/stackvault/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/MB512/stackvault/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/MB512/stackvault/releases/tag/v0.1.0
