# Changelog

All notable changes to StackVault. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), versions follow
[Semantic Versioning](https://semver.org/).

## [Unreleased]

### Upgrading

Nothing to change in the compose file. The database is migrated at
startup (0014 and 0015 drop schedule options that no longer exist, 0016
the stack-level enabled flag: the schedules of a disabled stack are
disabled instead; 0017 records which stacks were on disk once). If you
set `STACKVAULT_MAX_JOBS=1`, consider removing it: the new default 3 lets
the pre-stop snapshots and restores of several stacks run at the same
time, while `STACKVAULT_MAX_DOWNTIMES` (default 1) still stops only one
stack at a time. `<stack>.restore-tmp` and `<stack>.old-<time>`
directories left by older versions can be deleted by hand. Logs of jobs
older than 90 days are deleted on the first start; set
`STACKVAULT_JOB_LOG_RETENTION=0` beforehand to keep them.

### Added

- **Colors in container logs** (#29): the ANSI colors applications write
  into their output are shown on the Logs tab and in the global log view;
  other escape sequences no longer appear as stray characters. "Colors"
  next to "Wrap lines" turns them off.
- **Private registries** (#14): a docker client configuration mounted
  into the manager at `/root/.docker` (or just its `config.json`) is
  passed read-only to every worker, so updates and pinned restores can pull
  private images; see "Private registries" in the README.
- **Old job logs are deleted** (#16): the job directories (log, status)
  of jobs that finished more than 90 days ago are removed
  (`STACKVAULT_JOB_LOG_RETENTION`, `0` keeps them); the jobs stay in the
  lists.

### Changed

- **Updates always skip when nothing is new** (#41): the option "only if
  there is something new" is gone, it is now how every update works. After
  the pull, an update without a new (or pinned) image ends without backup,
  stop or recreate. The worker flag `--only-if-new` was removed.
- **Every restore runs in place** (#33): only files that differ from the
  snapshot are downloaded and rewritten, files not in the snapshot are
  deleted. The "switch over" method (restore into `<stack>.restore-tmp`,
  keep `<stack>.old-<time>`) and the "roll back in place" options of
  updates, the compose editor and update schedules are gone; automatic
  rollbacks run in place too. Left-over `.restore-tmp` and `.old-*`
  directories of older versions are still ignored by discovery and can be
  deleted by hand.
- **Interrupted restores continue** (#33): a restore or import that stops
  half-way (timeout, network error, cancel) keeps what it restored and
  marks the stack directory (`.stackvault-restore-incomplete`). Restoring
  again continues it, downloading only what is missing, without a safety
  snapshot of the partly restored directory. Until a restore completes,
  the stack shows "restore incomplete" and backups, updates, compose
  changes and starts are refused.
- **The safety snapshot before a restore is mandatory** (#33): admins can
  no longer skip it (it is the only way back from an in-place restore),
  and the worker flag `--no-safety-snapshot` is gone. Only an automatic
  rollback restores without one.
- **Every backup takes a pre-stop snapshot** (#34), also updates and
  compose changes: a snapshot while the stack runs, then the final one with
  the stack stopped, so it is only down for the changes in between. The
  options "Snapshot before stopping" (backup, update, schedules) and the
  worker flags `--pre-stop` / `--no-pre-stop` are gone; migration 0015
  drops `schedules.pre_stop`.
- **Snapshot list without pre-stop snapshots** (#35): each backup's row
  shows what the backup added and how long it took in total (pre-stop and
  final snapshot together); the tooltip shows how long the stack was
  stopped. "Show pre-stop snapshots" lists them on their own. Restoring a
  pre-stop snapshot is admin-only and warns that it may not be consistent.
- **Downtime slots** (#36): only the part of a backup, update or compose
  apply that keeps a stack stopped is limited by the new
  `STACKVAULT_MAX_DOWNTIMES` (default 1). The pre-stop snapshot before it
  and restores need no slot, so a long backup or restore no longer holds up
  the other stacks. `STACKVAULT_MAX_JOBS` now defaults to 3 and limits all
  running jobs (disk and network load), restores included: the exception
  for restores into a stopped stack is gone.
- **Downtime timeout** (#36): `STACKVAULT_DOWNTIME_TIMEOUT` (default 1h)
  bounds stop -> final snapshot; when it runs out, the final snapshot is
  aborted, the stack started again and the job fails.
- **No more "Disable stack"** (#37): schedules are enabled or disabled on
  their own; the Settings tab has "Enable all" and "Disable all" for a
  stack's schedules. Migration 0016 disables the schedules of stacks that
  were disabled and drops `stacks.enabled`.
- **New stack list** (#37): backup and update column groups (scheduling
  with the next run, repository, last backup; last check, last update),
  times as "4 h" / "25 d" with the exact time in the tooltip, and status
  dots (✓ / ✗ / running). A failed last backup also shows when the last
  successful one was; "update available" appears when the last check found
  newer images. "Last update" counts updates and compose changes that
  replaced an image; the backup group's "Last" is the newest consistent
  snapshot, also when an update or compose change made it.
- **Stacks without directory are hidden** in the stack list (#27); "Show
  stacks without directory (N)" lists them again. A stack imported from
  another host that is not restored yet stays visible, marked "not
  restored yet". Migration 0017 adds `stacks.seen_present`.
- **Job timeout** raised from 6 h to 48 h and configurable on the manager
  with `STACKVAULT_JOB_TIMEOUT` (e.g. `72h`; #32). Before, the variable was
  only read by the worker and the manager did not pass it on, so large
  restores were always stopped after 6 h.

### Fixed

- **Cancelled and timed-out jobs** were shown as failed with restic's last
  output as the error (e.g. `restic restore: signal: killed`; #30). A
  cancelled job is now `cancelled`, and a job stopped by the job timeout
  says `job timeout (…) exceeded`.
- A cancelled or timed-out job **killed restic**, which left its lock in
  the repository (an append-only repository cannot remove it; #30). restic
  is now interrupted like Ctrl+C and killed only after 30 s.
- The job log showed the **snapshot's time instead of the log time** for
  the line "snapshot verified" of a restore (#31).
- After an upgrade, **browsers kept the old scripts and styles** for up to
  an hour (#20). Static files are now referenced with their content
  version and loaded anew as soon as they change.

## [0.4.0] - 2026-11-18

### Upgrading from 0.3

Replace the image tag `0.3` with `0.4` and run `docker compose up -d` in the
StackVault stack. The database is migrated at startup (0011 rebuilds the
`jobs` and `schedules` tables, 0012 and 0013 add columns). Nothing else to
change; the new setting `STACKVAULT_CLEANUP_IMAGE_MIN_AGE` is optional.
Sizes and durations appear for snapshots once the snapshot list was
refreshed (and only for snapshots made with restic 0.17 or newer).

### Added

- **Move a stack to another host** (or rebuild a host) through its
  repository (#1). Admins import a stack on the "Import stack" admin tab:
  enter the repository, choose one of the stacks and hosts found in its
  snapshots. Its first restore creates the stack directory (nothing to
  stop, no safety snapshot), started only if asked; later restores of the
  source host's newer snapshots sync it in place, until the final one
  after the last backup on the old host. Per stack, the Settings tab
  chooses the source host whose snapshots are listed and restored
  (restoring another host's snapshots is admin-only); backups always use
  this host's name. Worker flags `restore --import` and
  `--limit-download`.
- Restores can **limit their download rate** (MiB/s), and restores into a
  stack that is not running run in the background: they do not count
  against `STACKVAULT_MAX_JOBS`, so a long one does not hold up scheduled
  backups.
- **Host cleanup** (#19) on the new admin "Host" tab: removes stopped
  orphan containers of the stacks (services no longer in the compose file)
  and unused untagged images last pulled or replaced by an update more
  than 7 days ago (`STACKVAULT_CLEANUP_IMAGE_MIN_AGE`). With a preview of
  what would be removed, "Clean up now" and schedules. Never prunes, never
  touches volumes; a cleanup runs alone. Worker command `worker cleanup`.
- **Admin pages** with tabs: Users, **Jobs** (running and queued jobs of
  all stacks and the recent jobs, live; #24), Host, Import stack and
  **Audit log** (paged, with user and stack names).
- **Up and Down** on the Status tab (operate right, without backup, with a
  confirmation): `docker compose up --detach` and `docker compose down`
  (volumes and files are kept). The controls now form the first row of
  actions (Up, Start, Restart, Stop, Down), backup and update the second.
  StackVault's own stack has no Down, like no Stop. Worker commands
  `worker up|down <stack>`.
- **Sizes and durations** (#9): the snapshot list shows what each snapshot
  added to the repository and how long the backup took (snapshots made with
  restic 0.17 or newer), and the stack size at the last backup. "Calculate"
  shows the stack's size on disk, also before the first backup. Job lists
  and job pages show how long a job ran.
- **Global log view** (#21): the "Logs" link in the header follows the
  container logs of up to 4 stacks in one view, in time order.

### Changed

- The account link in the header reads "Profile" instead of the user name
  with "(admin)".
- Update and compose-apply jobs record the image ids they replaced
  (kept by the host cleanup for a while).

### Fixed

- A compose change that removed a running service failed and was rolled
  back ("no such service"; #26). Containers of services that are no longer
  in the compose file (orphans) are now left alone by every job: they are
  not started again after a backup, restore, update, start or restart, and
  not recorded in the snapshot's `metadata.txt`.
- Services added to the compose file while the stack was stopped were
  never started: "Start" only started existing containers, and the next
  compose change counted them as known. Start now also creates and starts
  services without a container (existing ones are not recreated), and a
  compose change starts them too.
- Schedules: "roll back in place" could be ticked for a backup schedule
  and was silently dropped; the option is now disabled for backups and
  refused by the server.

## [0.3.0] - 2026-11-17

### Upgrading from 0.2

Replace the image tag `0.2` with `0.3` and run `docker compose up -d` in the
StackVault stack; the database is migrated at startup. Three things to check:

- **`TZ` must be a valid time zone** (e.g. `Europe/Berlin`). An unknown
  zone now stops the start instead of silently falling back to UTC. All
  times in the UI are now shown in this zone.
- **New per-stack right "Operate"** (stop, start, restart). Existing users
  do not have it; admins grant it on the stack's Settings tab. Admins
  always have it.
- **Stacks created with the old docker-compose v1** (Python,
  `docker-compose`) are now refused by every job with a clear message and
  shown as "legacy (compose v1)" in the stack list. Recreate such a stack
  once with `docker compose up -d` in its directory.

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
- Stack list: a new "State" column shows whether a stack is started,
  stopped, degraded (a service not running, unhealthy or restarting), not
  created, busy with a job, missing or created by docker-compose v1. It
  loads after the page and again with "Refresh state".
- Stack list: the "Status" column is now "Scheduling": `active` (at least
  one enabled backup or update schedule), `none` or `disabled` (the stack
  is disabled); the columns "Snapshots", "Last snapshot"
  and "You may" are gone. A new "Last backup" column shows the newest
  backup job with its state, linking to it; its tooltip gives the time of
  the last successful backup. "Last update check" shows the newest update
  job the same way; its tooltip gives the last update that actually
  changed the image of a running container (issue #23).
- Update and compose-apply jobs record which services run another image
  afterwards (`images_changed` in the job result).
- Stacks whose containers were created by the old Python docker-compose v1
  are refused with a clear message ("recreate them once with `docker
  compose up -d`") instead of "containers from another directory". This
  also covers v1 containers under the project name older v1 versions
  derived (`myapp` for a directory `my-app`), which were not detected
  before.

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

[Unreleased]: https://github.com/MB512/stackvault/compare/v0.4.0...HEAD
[0.4.0]: https://github.com/MB512/stackvault/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/MB512/stackvault/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/MB512/stackvault/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/MB512/stackvault/releases/tag/v0.1.0
