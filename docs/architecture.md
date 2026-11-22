# Architecture

## Components

```
 Browser ──HTTP──> manager (container, long-running)
                     │  web UI, auth, RBAC, scheduler, SQLite
                     │  creates job spec + starts worker via Docker socket
                     ▼
                  worker (container, one-shot, started per job)
                     │  backup / restore pipeline
                     ├── docker compose (stop/start/ps/images)
                     └── restic (backup/restore/snapshots, append-only safe)
```

Both are binaries from the same image (`/app/manager`, `/app/worker`).
The image contains `restic`, `docker-cli` and the `docker compose` plugin.

### Why a separate worker

- The manager's own stack can be backed up and restored: the worker is not part
  of any compose project it stops, so it survives.
- A crashing/restarting manager does not kill a running backup.

## Repository assumptions

Repositories are served by `restic/rest-server` in **append-only mode**. The
app therefore only uses operations that work there: `init`, `backup`,
`restore`, `snapshots`, `ls`, `cat config`, and read-only `check`. It never calls `forget`,
`prune` or `tag`, which would fail or are unwanted. Retention and pruning are
the responsibility of the repository owner on the server. Deleting stale lock
files is allowed in append-only mode.

## Mounts

Manager:

- `STACKS_ROOT` (e.g. `/home/docker`) mounted at the **same absolute path** as
  on the host (`/home/docker:/home/docker`). Needed so relative paths in compose
  files resolve identically for the host daemon.
- `/var/run/docker.sock` (to create workers and read stack state).
- `/data` for DB and job directories (this lives inside the manager's own stack
  folder, so it is part of the self-backup).

Worker (started by manager):

- `STACKS_ROOT` at the same path (read-write; restore replaces directories, so
  mount the **parent**, not individual stack folders).
- `/var/run/docker.sock`.
- `/job` (bind of `/data/jobs/<id>`): job spec in, status/log out.
- Restic cache volume (shared, named volume).
- Run as root (needed to preserve ownership/permissions on backup and restore).

Workers carry a label `stackvault.worker=true` and are NOT created through
compose, so they are never affected by stopping a stack.

## Manager configuration

| Variable | Meaning |
|---|---|
| `STACKS_ROOT` | absolute stacks root, mounted at the same path as on the host (required) |
| `STACKBELAY_HOSTNAME` | name of the Docker host, passed to workers for `restic --host` (required) |
| `STACKBELAY_SECRET_KEY` / `STACKBELAY_SECRET_KEY_FILE` | 32-byte key, base64 (`openssl rand -base64 32`); at most one may be set. Neither set: `<STACKBELAY_DATA_DIR>/secret.key`, created on a fresh install. See "Secret key" |
| `STACKBELAY_DATA_DIR` | database and job directories, default `/data` |
| `STACKBELAY_LISTEN` | listen address, default `:8080` |
| `STACKBELAY_SESSION_TTL` | session lifetime, default `12h` |
| `STACKBELAY_ALLOW_INSECURE_HTTP` | accept logins over plain HTTP (testing without TLS proxy), default `false` |
| `STACKBELAY_WORKER_IMAGE` | image for worker containers, default: the manager's own image |
| `STACKBELAY_MAX_JOBS` | jobs running at the same time across all stacks (disk and network load), default `3`; restores count too (issue #36) |
| `STACKBELAY_MAX_DOWNTIMES` | jobs that may keep a stack stopped at the same time (downtime slots, see "Downtime slots"), default `1` |
| `STACKBELAY_DOWNTIME_TIMEOUT` | limit for stop -> final snapshot of a backup, update or compose apply (Go duration, at least `1m`), default `1h`; passed to every worker |
| `STACKBELAY_JOB_TIMEOUT` | hard limit for one job (Go duration, at least `1m`), default `48h`; passed to every worker (issue #32) |
| `STACKBELAY_JOB_LOG_RETENTION` | job directories (logs) of jobs finished longer ago are deleted, the job rows stay; Go duration, at least `24h`, default `2160h` (90 days), `0` keeps them (issue #16) |
| `STACKBELAY_CLEANUP_IMAGE_MIN_AGE` | host cleanup: keep dangling images pulled, tagged or replaced by an update less than this long ago, whole hours, default `168h`; see "Host cleanup" |
| `STACKBELAY_ENABLE_SHELL` | admin-only container shells ("Shell" per container on the Status tab), default `false`; see "Container shell" |
| `STACKBELAY_LOG_LEVEL` | `debug`, `info`, `warn`, `error` |
| `TZ` | time zone of the cron schedules and of every time the UI shows (e.g. `Europe/Berlin`), default UTC in most images; the zone database is built into the binary; an unknown zone is a configuration error (Go would silently use UTC) |

The database file is created with mode 0600. `/healthz` checks the database.
The image's `HEALTHCHECK` runs `manager healthcheck`, which calls it on the
address from `STACKBELAY_LISTEN`, so compose files need no `healthcheck:`
(one there still overrides it). Workers are started with `--no-healthcheck`. Stacks are discovered at
startup and mirrored into the database.

### Secret key

The key encrypts repository credentials in the database (AES-256-GCM, see
`internal/secrets`). Sources, in this order:

1. `STACKBELAY_SECRET_KEY` (base64 in the variable) or
   `STACKBELAY_SECRET_KEY_FILE` (path of a file containing it). Setting
   both is a configuration error. If `<data>/secret.key` exists as well, it
   is ignored with a warning.
2. Neither set: `<STACKBELAY_DATA_DIR>/secret.key`.

The format is the same everywhere: base64 of 32 bytes, surrounding
whitespace ignored. The data-dir key file is **generated only on a fresh
install**: no key source set, `secret.key` missing **and** `stackvault.db`
missing. It is created with `O_EXCL` and mode 0600, file and directory are
fsynced before the key is used, and the path (never the key) is logged once.
If the database exists but no key is found, the manager refuses to start: a
new key would silently make the stored credentials unreadable. A key file
readable by group or others is accepted with a warning.

**Key check.** After the migrations, `store.Open` verifies the key against
the `meta` row `key_check` (the text `stackvault key check` sealed with
associated data `stackvault:key-check`). If it does not decrypt, startup is
refused with "the secret key does not match this database". A database
from 0.1 has no such row: if it has a repository, that secret must decrypt
with its own associated data and the row is written; if it fails, startup is
refused. Without repositories nothing was encrypted yet and the row is
written right away.

**Backups.** With the data directory inside the manager's stack (as in
`deploy/compose.example.yml`), `secret.key` is part of that stack's
backups, next to the database. They are protected by the restic repository
password, which is not stored on the host in plain text and must be known to
restore anyway. Restoring the manager's stack restores the matching key and
database together. To keep the key out of every backup, use
`STACKBELAY_SECRET_KEY_FILE` with a file outside `STACKS_ROOT` (e.g.
`/etc/stackvault/secret.key`). Workers never receive the key: it is not in
their environment, and their only data-dir mount is their own job directory.

## Job handoff

The manager writes `/data/jobs/<id>/spec.json` and starts the worker with
`worker run /job` (`/job` = bind of the job directory). The spec contains
only the job (version, id, type, stack, options; see `jobs.Spec`) and is
validated strictly by the worker (unknown fields rejected). Repository
location and credentials are passed as environment variables of the worker
container (`RESTIC_REPOSITORY`, `RESTIC_PASSWORD`, backend variables). The
manager sets them with `docker run -e NAME` (name only), so the values come
from the docker CLI's own environment and never appear in a command line or
a file. The worker writes `status.json` atomically (state, timestamps,
snapshot ids, error; see `jobs.Status`) when the job
starts and when it ends, `log.txt` as JSON lines, and `progress.json`
while it runs (see "Job progress"). Both log and error are
filtered for the credential values. If the spec is invalid, the worker
still writes a `failed` status with the reason. The manager reads these files
and mirrors them into SQLite. Files, not the DB, are the handoff channel
because the manager (and its SQLite file) may be stopped during a self-backup.

On startup the manager reconciles the job directories with the DB
(`Dispatcher.Reconcile`, two passes): jobs the DB still has as queued or
running whose directory holds a final status are finished from it; job
directories the DB does not know are imported (spec + status; the spec
carries trigger and requester for this; a directory without status becomes
`failed`). Audit entry `job.import`.

**Self-restore.** The manager's jobs directory lies inside its own stack
(`/data` is part of it). Restoring that stack would bring back older job
files (and delete the newer ones) along with an older database. So for a
restore of the stack that contains the jobs directory (and for an update
or compose apply with backup, whose rollback is such a restore), the spec
carries `carry_over_jobs` (the jobs directory relative to the stack, e.g.
`data/jobs`), which the restore excludes: the job files are left as they
are. The
restarted manager then reconciles: the self-backup that was running when
the snapshot was taken is finished from its real result, and the restore
itself (and any later jobs) are imported. Users, repositories and
permissions are those of the snapshot, as expected from a restore.

Job states: `queued -> running -> succeeded | failed | cancelled`.
At most one job per stack at a time, enforced by a partial unique index in
SQLite (see data-model.md); further jobs queue. A stack has at most one
queued job per type.

### Dispatcher (`internal/dispatch`)

A loop in the manager (every 2 s, and immediately when a job is queued):

1. For every `running` job: a final `status.json` finishes it (result into
   the DB, audit entry `job.finish`, container removed, snapshot cache
   refreshed if snapshots were created). A worker container that stopped or
   vanished without a final status fails the job ("exit code N").
2. Grant downtime slots (see "Downtime slots").
3. Claim queued jobs (oldest first, stack not locked, at most
   `STACKBELAY_MAX_JOBS` running in total), write `spec.json`, start the
   worker container. Start errors (no repository, `docker run` failed) fail
   the job and release the lock. Stop, start, restart and host cleanup
   jobs get no repository credentials at all. For a cleanup the spec gets
   the minimum image age and the images to keep (see "Host cleanup").

### Downtime slots

Decided in issue #36: only the part of a job that keeps a stack stopped
holds up other stacks. A backup, update or compose apply of a running stack
first takes its pre-stop snapshot (stack still running, no slot needed),
then asks for a **downtime slot** before it stops the stack: the worker
writes `downtime-request` into its job directory, the job page shows
"waiting for a downtime slot", and the dispatcher writes `downtime-granted`
for the oldest waiting jobs (job ids sort by creation) while fewer than
`STACKBELAY_MAX_DOWNTIMES` running jobs hold one (`grantDowntimes`). A slot
is held until the job ends. The state lives only in the job directories
(`jobs.DowntimeState`), so it survives a restart of the manager, also when
the slot was granted to the backup of the manager's own stack. Cancelling a
job that waits for its slot is a normal cancel: nothing was stopped, so
nothing is started again. Restores, imports, stop, start, restart, up,
down and cleanups need no slot; a worker run by hand (`worker backup ...`)
has no gate at all.

`STACKBELAY_DOWNTIME_TIMEOUT` (default `1h`) bounds stop -> final snapshot:
when it runs out, the final snapshot is aborted, the stack is started
again (as after any failure) and the job fails ("downtime timeout ...
exceeded"); the pre-stop snapshot is kept. Waiting for the slot does not
count. The start after a backup, the change and health wait of an update
or compose apply and their rollback keep their own limits.
`STACKBELAY_MAX_JOBS` (default `3`) still limits all running jobs,
restores included, for the disk and network load.

All state is in the DB and the job directories, so a manager that was
stopped by its own backup continues where it left off: running jobs are
simply checked again. The worker container is removed *before* the result
is recorded, so a manager stopped in between finishes the job on the next
check instead of leaving a container behind.

The worker runs as root; files it writes into the job directory
(`status.json`, `log.txt`) get the owner of that directory (the manager's
user), so a manager not running as root can still read them.

**Docker Desktop with WSL integration is not supported for running jobs.**
Docker Desktop translates WSL bind-mount paths per container; a stack that
the worker stops through the raw socket loses these mounts and cannot be
started again (only recreated). Use Docker Engine on Linux (or inside WSL
without Docker Desktop). Stacks without bind mounts are not affected. Cancelling a queued job marks it `cancelled`;
cancelling a running job sends SIGTERM (never SIGKILL) to the worker, which
restarts the stack and reports `cancelled`. When a job is cancelled or
runs into the job timeout, a running restic gets SIGINT (killed only if it
has not exited after 30 s), so it removes its lock from the repository: in
an append-only repository a client cannot remove a stale lock.

Worker container template (fixed, nothing from user input):

```
docker run --detach --name stackvault-job-<id>
  --label stackvault.worker=true --label stackvault.job=<ID>
  --mount type=bind,source=$STACKS_ROOT,target=$STACKS_ROOT
  --mount type=bind,source=<host docker socket>,target=/var/run/docker.sock
  --mount <host jobs dir>/<ID> -> /job   (bind, or volume with volume-subpath)
  --mount type=volume,source=stackvault-restic-cache,target=/cache
  [--mount <manager's /root/.docker or /root/.docker/config.json>,readonly]
  --env STACKS_ROOT --env STACKBELAY_HOSTNAME --env RESTIC_CACHE_DIR
  --env STACKBELAY_JOB_TIMEOUT --env STACKBELAY_DOWNTIME_TIMEOUT [--env STACKBELAY_LOG_LEVEL]
  --env RESTIC_REPOSITORY --env RESTIC_PASSWORD --env <backend variables>
  <image> /app/worker run /job
```

The manager finds the host side of its mounts and its own image by
inspecting its own container (id from `/proc/self/mountinfo`). If the
manager has the docker client configuration mounted at `/root/.docker`
(directory) or `/root/.docker/config.json` (file), every worker gets the
same mount read-only (`dockerConfigMount`, issue #14): the registry
credentials of `docker login` for pulls of private images (update, pinned
restore, starts that pull); the manager's update check uses them too.
`STACKS_ROOT` must be bind-mounted from the same path; `/data` may be a
bind mount or a named volume (Docker >= 26 for `volume-subpath`). Outside a
container (development) paths are used as they are and
`STACKBELAY_WORKER_IMAGE` is required. The docker CLI gets an allow-listed
environment (`PATH`, `HOME`, `DOCKER_*`) plus the job's credentials, which
`--env NAME` forwards by name.

**Orphans** (issue #26): `docker.CLI.Services` lists the services of the
compose file and the project's containers; a container whose service is
no longer in the file (removed from it while its container existed) is
listed with `Defined = false`. Compose refuses to start, recreate or pull
such a service ("no such service"), so every pipeline reads the stack
through `Pipeline.services`, which leaves orphans out and logs them: they
are not stopped-and-started by a backup, restore, update, compose change,
start, restart or up, and not recorded in `metadata.txt`. After a
restore, the services `metadata.txt` lists as running are only started if
the restored compose file defines them (older snapshots may list an
orphan). Stopped orphans are removed by the host cleanup; `compose stop`
does not stop a running orphan.

## Backup pipeline

1. **Preflight**: stack exists in discovery, repo reachable (`restic cat config`),
   lock acquired.
2. **Record state**: for each service, record whether it is running, plus the
   image reference, the image ID and the **repo digest(s)**
   (`docker compose ps --format json`, `docker image inspect` ->
   `.Id` and `.RepoDigests`). Write `metadata.txt` into the stack folder (it is
   then part of the snapshot). See "metadata.txt format" below.
3. **Pre-stop snapshot** (always, issue #34; skipped only if no service
   is running): stack still running, `restic backup` with tag `pre-stop`.
   Not guaranteed consistent; its purpose is to make the final run fast
   (parent snapshot, small delta), so the stack is only down for the
   changes in between. Failure here is non-fatal: log a warning and
   continue without a parent.
4. **Stop**: `docker compose stop` (not `down`, containers and networks are
   preserved). Remember which services were running.
5. **Final snapshot**: `restic backup --parent <pre-stop-id> --tag final`
   of the stack directory. This is the snapshot that counts.
6. **Start (always)**: in a `defer`, `docker compose start` only for services
   that were running in step 2. Wait for health (bounded timeout), report
   unhealthy services, but do not fail the backup because of them.
7. **Finish**: write final status, release lock.

Self-backup: identical. The worker stops the manager's own stack; the manager
disappears, the worker finishes and restarts it, the manager reconciles the job.

Error handling rules:

- Anything after step 4 must still reach step 6.
- Context cancellation and a hard job timeout both trigger step 6.
- If `docker compose start` fails, the job is `failed` with a loud error; the
  stack is left as is (no further automatic actions).

### metadata.txt format

Human-readable but strictly parseable, versioned, one record per line, tab
separated, so the restore can rely on it:

```
# stackvault-metadata v1
created_at     2026-11-12T20:15:00Z
stack          nextcloud
compose_sha256 <hash of the main compose file>
# service  state    image_ref            repo_digest                      image_id        container_id
app        running  nextcloud:29         nextcloud@sha256:ab12...         sha256:cd34...  9f8e...
db         running  postgres:16          postgres@sha256:ef56...          sha256:7890...  1a2b...
web        exited   -                    -                                sha256:...      ...
```

- `repo_digest` is what can be pulled again (`name@sha256:...`). The image ID
  is only a local identifier and is NOT pullable; it is kept for diagnostics
  and to detect whether the exact image is still present locally.
- Images built locally (`build:`) have no repo digest: write `-`.
- Services without a container (never created) are recorded with `-`.

## Restore pipeline

Every restore runs **in place** (issue #33): the snapshot is restored over
the stack directory itself, there is no second copy and no directory swap.
Only files that differ from the snapshot are read from the repository, so
a restore of a large stack is fast and needs no room for a second copy, and
a restore that stopped half-way (timeout, network error, cancel) continues
where it stopped when it is run again.

1. **Preflight**: permission re-checked by the manager, snapshot exists, lock.
2. **Stop** the stack.
3. **Safety snapshot**: back up the stopped (consistent) stack with tag
   `pre-restore`. It cannot be turned off (issue #33: it is the only way
   back from an in-place restore). Skipped only for an automatic rollback
   (its final snapshot was taken minutes before) and when the last
   restore did not complete (see below: it would only save a partly
   restored directory).
4. **Restore**: create the marker file `.stackvault-restore-incomplete`
   in the stack directory, then
   `restic restore <snap>:<snapshot path> --target <root>/<stack> --delete
   --overwrite if-changed --exclude /.stackvault-restore-incomplete
   [--exclude /<carry_over_jobs>]` (restic >= 0.17). Files with the same
   size and modification time as in the snapshot count as unchanged and
   are neither read nor written; the others are verified and rewritten
   where their content differs; files that are not in the snapshot are
   deleted (excluded paths are neither restored nor deleted). The
   `<snap>:<subfolder>` syntax restores the *contents* of the snapshot's
   stack directory directly into the target, without recreating the
   absolute path (`<target>/home/docker/<stack>/...`). `<snapshot path>` is
   taken from the snapshot's `paths[0]`, so it works regardless of the
   source host's `STACKS_ROOT`. Then the compose file is checked, and the
   metadata of the stack directory itself (owner, mode, mtime; restic does
   not restore it with this syntax) is read with
   `restic ls --json <snap> <snapshot path>` and applied; the marker is
   removed just before the mtime is set.
5. **Start** the stack (services that were running at snapshot time, taken from
   `metadata.txt`; fallback: all) with `docker compose up -d --force-recreate`.
   Recreating is required: containers must pick up the restored compose
   configuration. Volumes are not removed. Image versions are chosen per
   the restore option "image versions" (see "Pinning image versions on
   restore").
6. **Verify** health. On failure, report.

Failure handling depends on whether restic started writing:

- Failure before that (stop, safety snapshot): the stack is started again
  as it was.
- Failure during the restore, before the directory is completely restored
  (restic, compose file check, directory metadata): the job fails, the
  stack **stays stopped** (starting a half-restored stack could damage its
  data further) and keeps the marker. The error says how to get back:
  restore again (continues the restore), or restore the safety snapshot.
- Failure after a complete restore but before the start (e.g. a pinned
  image that cannot be pulled): the stack stays stopped with the restored
  files.
- Failure of the start: reported like any failed start.

**Incomplete restore** (`stacks.IncompleteMarker`): while the marker
exists, the directory may be partly restored. Backup, update, compose
apply, start, restart and up refuse the stack (`requireComplete`), so a
partly restored stack is neither started nor recorded as a valid snapshot;
stop and down are allowed. The next restore continues the incomplete one
without a safety snapshot. Every stack page shows a "restore incomplete"
badge and warning, the restore page says that the restore continues the
incomplete one. The safety snapshot of the restore that did not complete
is the way back to the state before it.

Self-restore works for the same reason as self-backup. Mount the parent
directory, never the stack folder itself.

Up to StackVault 0.4 a restore went into `<stack>.restore-tmp` and swapped
it in, keeping the previous directory as `<stack>.old-<time>`. Directories
with these names are still never discovered as stacks (see "Stack
discovery"); left-over ones can be deleted by hand.

## Restore from another host

Decided in issue #1: moving a stack from host A to host B (or rebuilding a
host) through the shared repository.

- **Import stack** (admin tab, `/admin/import`): the admin enters a
  repository (location, password, backend variables, as on the Settings
  tab). The manager lists its snapshots read-only (`restic snapshots`,
  `web.RepoSnapshotLister`) and offers every stack name (last component of
  a snapshot's single path, `stacks.ValidName`) and host, with the number
  of snapshots and the newest. Names with a stack record or a directory on
  this host are shown disabled. The credentials wait on the server for the
  second step (`pendingImports`: 15 minutes, bound to the admin, used
  once); the page only carries a random token and the index of the choice.
  The import creates the stack record (`CreateImportedStack`: not present,
  no schedules, source host = the chosen host), saves the repository, audits
  `stack.import` and fills the snapshot cache. Only admins see the new
  stack until permissions are granted. The stack keeps its name: the
snapshot path's last component must equal it (`snapshotStackPath`), the
rest of the path may differ (`/home/docker/web` on A restores into
`$STACKS_ROOT/web` on B).

- **First restore (import)**: `Pipeline.Import`, job option / spec field
  `import`. The stack directory does not exist (`stacks.ImportTarget`:
  valid name, no directory), so there is nothing to stop or to protect
  with a safety snapshot. The worker creates the directory (`mkdir`, which
  never replaces anything that appeared in the meantime) with the
  incomplete-restore marker and restores into it like any restore (see
  "Restore pipeline"); then the image digests in `metadata.txt` are
  compared with the local images (missing ones are logged; a start pulls
  them, by digest with pinned image versions). It starts only if asked;
  with "pin image versions" (recommended for migrations).
- **Incomplete import** (issue #33): a failed import keeps what it
  restored, with the marker. `stacks.ImportTarget` also accepts such a
  directory (a real directory, not a symlink, containing the marker), so
  running the restore again continues the import: only missing and changed
  files are downloaded. A long first import over the internet does not
  start from zero after a timeout or a network error. Once the compose
  file is restored, discovery may already list the stack as present; its
  next restore is then a normal restore that continues the incomplete one
  (no safety snapshot of the partly restored directory).
- **Later syncs** are normal restores of the source host's newer
  snapshots (only changed files are read and written), not started; the
  last one after the final backup on host A with start and pinned images.
  So a large stack moves with repeated, ever faster syncs and a short
  final downtime.
- **Source host** (`stacks.source_host`, Settings tab): whose snapshots
  the Backups tab lists and the restore accepts (`StackSnapshots` with
  that host instead of `STACKBELAY_HOSTNAME`). Offered are this host and
  the hosts with cached snapshots of the stack. Restoring another host's
  snapshots is admin-only (restore links hidden, form and action answer
  403). Backups are always made under this host's name.
- **Restore page**: for a stack without directory it offers no safety
  snapshot (the restore creates the directory), "start" off and pinned
  image versions on; for another host's snapshot it preselects "do not
  start" and pinned versions, and lists the
  caveats (numeric UID/GID restored as is; ports, external networks and
  absolute paths outside the stack directory may not fit; only bind mounts
  inside the stack directory are in the snapshot).
- **No downtime slot**: a restore needs none (see "Downtime slots"), so a
  long first restore does not hold up the scheduled backups of other
  stacks; it counts against `STACKBELAY_MAX_JOBS` like every job.
- **Download limit**: restore option `limit_download_kib` (worker flag
  `--limit-download`), passed to every restic command of the job as
  `--limit-download` (KiB/s, at most `restic.MaxLimitKiB`).

## Pinning image versions on restore

Goal: after a restore, run the image versions that were running at backup
time, not whatever the tag (`:latest`, `:29`, ...) points to today.

Restore option **image versions**:

- `tags` (default): start as defined in `compose.yml`. Tags resolve to current
  images (may pull newer ones).
- `pinned`: use the repo digests from `metadata.txt`.

How `pinned` works (`jobs.Pipeline.preparePinning`; the stack's own
`compose.yml` is never modified). Steps 1–3 run after the restore and
before the start, so any failure (no or invalid `metadata.txt`, an image
that cannot be pulled, `COMPOSE_FILE` in `.env`) leaves the stack stopped
with the restored files (see "Restore pipeline").

1. Parse `metadata.txt` from the restored directory (`ParseMetadata`, every
   field validated; repo digests must match the digest syntax).
2. For each service with a repo digest: `docker.Client.EnsureImage` — keep
   it if `docker image inspect` finds it, else `docker pull name@sha256:...`.
   Services without a digest (built locally, never created) are logged as
   not pinnable and use `compose.yml`.
3. Generate an override file in the worker's temp directory (written as
   JSON, which is valid YAML; removed after the job):
   ```yaml
   services:
     app:
       image: nextcloud@sha256:ab12...
       pull_policy: never   # already pulled in step 2
   ```
4. Start with `docker compose --project-directory <stack> --file <main>
   [--file <override>] --file <pinned override> up -d --force-recreate`.
   Passing `--file` disables compose's own file lookup, so the worker
   repeats it: the main file is the first of `compose.yaml`, `compose.yml`,
   `docker-compose.yml`, `docker-compose.yaml` (`stacks.MainFileNames()`,
   the same file stack discovery picks); the override the first of `compose.override.yml`, `compose.override.yaml`,
   `docker-compose.override.yml`, `docker-compose.override.yaml`. A stack
   whose `.env` sets `COMPOSE_FILE` cannot be pinned (the files are then
   unknown).

With "do not start" the option has no effect (nothing is pulled).
UI: radio buttons "Image versions" on the restore page (default: tags);
job option `pin_images`, worker flag `--pin-images`.

Notes and limits (show them in the UI):

- Services with `build:` and no repo digest cannot be pinned. They are rebuilt
  from the restored sources; base images may have changed. Warn per service.
- The registry must still serve that digest. Registries can delete old
  manifests. If a pull fails the restore is aborted (rolled back); the user
  can retry with `tags`.
- Private registries: the worker needs registry credentials (mount a Docker
  `config.json` read-only into the worker if required).
- Compose `up -d` later, run manually with only `compose.yml`, will detect an
  image change and recreate containers with the tag version.
- Multi-arch: the repo digest recorded from `docker image inspect` identifies
  the manifest that was pulled; pulling by it yields the same platform content.
- Without `metadata.txt` (very old snapshots) a pinned restore fails before the start (the UI cannot know in advance without reading the snapshot).

## Update pipeline

Job type `update` (`jobs.Pipeline.Update`), started by users with the update
right (`can_update`) from the update page, which the Status tab links to
with **Update…** (see "Update check"). It runs in
a worker like backup and restore (same queue, per-stack lock, live log,
cancel):

1. With a repository: check it. Record the stack state; the stack must have
   running services (an update recreates the running services; a stopped
   stack is refused before anything happens). Write `metadata.txt`: its
   image digests come from the running containers, i.e. the versions
   **before** the update.
2. `docker compose pull --ignore-buildable --quiet -- <running services>`
   while the stack keeps running. Failure: the job fails, nothing changed.
   The worker then compares, per running service, the id of the local image its compose file names
   (`docker image inspect <ref>`, i.e. what `up` would use) with the
   container's image id, and the container's image reference with the
   compose file's. A service also counts as changed when its container
   runs another reference, e.g. a version pinned by digest by a restore or
   rollback: recreating it switches back to the tag ("unpin"). If no
   service changes, the job ends here as `succeeded` with
   `result.unchanged`: no snapshots, nothing stopped. Services without an
   image (built locally) do not count. This check is not optional (issue
   #41): an update without a new image would only cause downtime. A
   `spec.json` from an older version may still carry `only_if_new`; it is
   accepted and ignored.
3. With a repository: pre-stop snapshot (always, issue #34; failure only
   warns).
4. `compose stop`.
5. With a repository: final snapshot (parent = pre-stop): the state the
   rollback returns to. Failure in 4–5 (or cancellation before 6): `compose
   start` of the old containers (still the old images), job fails.
6. `compose up --detach --force-recreate -- <running services>`, then wait
   until every service runs and, with a healthcheck, is healthy (5 min;
   a service that exits or turns unhealthy fails at once). On success the
   worker reads the services again and records in `result.images_changed`
   the services that ran before and now run another image id (content
   digest; also for a compose apply). Whether the job pulled the image
   does not matter: another stack's update may have pulled it before. A
   job with a non-empty list is a "real" update for the stack list's
   tooltip (issue #23); a failure to read the state only warns. The image
   ids those services ran before go into `result.replaced_images`: the host
   cleanup keeps them for a while (see "Host cleanup").
7. Failure in 6:
   - with repository: **automatic rollback** = the restore pipeline on the
     final snapshot without safety snapshot (the final snapshot already is
     the state before the update), with image pinning (old digests from its
     `metadata.txt`), leaving the manager's job files alone if it is the
     manager's own stack. Like every restore it runs in place: only the
     few files changed since the final snapshot are rewritten, nothing of
     the failed version is kept, and a rollback that fails half-way leaves
     the stack stopped and marked as incomplete (see "Restore pipeline").
     The rollback also runs if the job was cancelled during step 6
     (own timeout, 2 h). The job ends `failed` with `result.rolled_back`.
   - without repository: **no rollback at all**, not even starting the old
     images (they could damage data a new version already migrated). The
     job fails and the stack stays as it is.

Without a repository the update is only possible after explicit
confirmation ("no backup and no rollback", `JobOptions.NoBackup`): on the
update page a required checkbox plus a confirmation dialog, for update
schedules when the schedule is created (see "Scheduler"). The dispatcher decides when the job starts: a stack
that has a repository by then is always backed up; one without repository
and without confirmation fails. The worker gets `no_backup` in the spec
explicitly; without it, missing repository settings fail the job instead of
silently skipping the backup.

A stack **with** a repository cannot be updated without backup: if the
repository is unreachable (e.g. the repository server or the connection to
it is down), every update of the stack, manual or scheduled, fails at step
1 once restic gives up retrying, before anything is pulled or stopped. The
updates have to wait until the repository is reachable again; the
"no backup" confirmation is offered only for stacks without repository
(discussion: issue #43).

Not covered: images built locally (`build:`) are not rebuilt; a service
that starts, passes the check and fails later is not detected.

### Update check

The Status tab has a **Check for updates** button for users with the
update right (`POST /stacks/{name}/update/check`, CSRF token because every
check sends registry requests; not audited, it changes nothing). The
result fills an "Update" column of the services table and stays there for
everyone who may view the stack until the next check (kept in memory per
stack, lost when the manager restarts); the 10-second refresh of the tab
keeps it. The update page shows the same summary. The manager
(`docker.CLI.CheckUpdates`) runs only read-only commands with fixed
argument lists:

1. `docker compose config/ps` (after the project owner check) and `docker
   image inspect` of each container's image: running version (repo
   digests, `org.opencontainers.image.version` label, creation date,
   platform).
2. `docker buildx imagetools inspect --format '{{json .}}' -- <ref>` per
   image reference of the compose file (at most 4 at a time, 2 minutes in
   total): asks the registry for the digest the tag points to now and the
   image config (label, date) for the container's platform. Nothing is
   pulled. Private registries need credentials in the manager's Docker
   client configuration; without them the service shows "unknown".
3. Per service: **up to date** (the registry digest is one of the
   container image's repo digests), **update available**, **unknown**
   (registry error, no container, image without registry digest) or **not
   checked** (built locally, pinned by digest in the compose file). A
   container started by a pinned restore or rollback
   (`name@sha256:...` instead of the tag) is compared by digest and marked
   as pinned. If running services are pinned, the update page offers
   **Unpin now** (with a repository): it queues an update,
   which recreates them with their tag (and, as any
   update, pulls newer images of the tags). The column shows running and available version: the version
   label if the image has one, else creation date and short digest.

At most one check per user and three in total run at a time. The check is
a preview: the update job pulls and compares again itself.
## Stop, start and restart

Job types `stop`, `start` and `restart` (decided in issue #18) and `up`
and `down`, queued from the Status tab (first row of actions, in the order
Up, Start, Restart, Stop, Down; backup and update in a second row) by users
with the operate right (`can_operate`; admins
always). They run in a worker like every change to a stack, take the
per-stack lock like every job (so they never run in the middle of a
backup, update or restore of the same stack), are audited (`job.create`)
and have a job log. They make **no backup** and need no repository: the
dispatcher passes no repository credentials to their worker.

- **Stop** (`jobs.Pipeline.Stop`): `docker compose stop`. Containers,
  networks and volumes are kept; never `compose down`, no
  `--remove-orphans`.
- **Start** (`jobs.Pipeline.Start`): `docker compose start` of every
  service that has a container (like `docker compose start` by hand), then
  the usual health wait (a warning only, as after a backup). Services that
  are defined but have no container yet (added to the compose file while
  the stack was stopped, e.g. with the compose editor) are then created
  and started with `compose up` for just those services, as `docker
  compose up -d` would; existing containers are not recreated. A stack
  without any containers fails ("create them once with `docker compose
  up`").
- **Up** (`jobs.Pipeline.Up`): `docker compose up --detach`, as by hand:
  missing containers are created, those whose configuration or image
  changed are recreated, the others are left alone (missing images are
  pulled by compose). Then the health wait for all services (a warning
  only: one-shot services exit).
- **Down** (`jobs.Pipeline.Down`): after the project owner check, `docker
  compose down`: containers and networks are removed; volumes, images and
  the stack directory are kept (never `--volumes`). Up creates the
  containers again.
- **Restart** (`jobs.Pipeline.Restart`): records the running services,
  stops the stack and starts those services again. The start is registered
  with `defer` before the stop, so it also runs when the stop fails or the
  job is cancelled. A stack with no running service is refused before
  anything is stopped (use Start).

The manager's own stack (`dispatch.Template.OwnStack`: its compose project
directory, from the label `com.docker.compose.project.working_dir` of its
container, is a stack directory) has **no Stop**: the button is disabled
with the reason (Down too), and `POST /stacks/{name}/stop` and `/down`
answer 409. Stopping it
would also stop the web UI, with nobody able to start it again from there.
Restarting it works like a self-backup: the worker stops and starts the
manager, which picks the job up again when it is back.

## Compose editor

The admin-only "Compose" tab edits the stack's main compose file (the one
discovery picked, see `stacks.MainFileNames()`); override files and `.env`
are not editable. It is admin-only because a compose file can mount any
host path and run privileged containers, i.e. it is equivalent to root on
the host. The editor is a plain `<textarea>` (no editor library, no
script); files over 256 KiB or that are not UTF-8 text are shown as not
editable.

"Validate and apply" (`POST /stacks/{name}/compose`, CSRF, admin):

1. Line breaks are normalized (browsers send CRLF). Refused with the text
   kept in the form: empty, larger than 256 KiB, not UTF-8, unchanged, or
   the file on disk changed since the editor was opened (the form carries
   the SHA-256 of the loaded version; the user reloads to see the current
   one, so no change is lost). Without repository the "no backup and no
   rollback" checkbox is required.
2. Validation in the manager (`docker.CLI.ValidateCompose`): the text in a
   temporary file **outside** the stack directory, checked with `docker
   compose --project-directory <stack> --file <tmp> config --quiet`, so
   `.env` and relative paths resolve as for the real file. Compose's message
   is shown in the form (with the real file name).
3. A `compose-apply` job is queued. The new content is stored in
   `job_compose` (not in the job's options), the options carry the file
   name and both checksums. The audit entry (`job.create`) has checksum and
   size, never the content. The manager never writes the file itself.

The dispatcher writes the content to `compose.new` in the job directory;
the worker checks it against `compose_sha256` from the spec and runs
`jobs.Pipeline.ComposeApply` on the same sequence as the update
(`runChange`, see "Update pipeline"):

- before any snapshot: the file on disk must still have the base checksum,
  otherwise the job fails with nothing changed; the current file is saved
  as `compose.previous` in the job directory (manual revert);
- with a repository: pre-stop snapshot, stop, final snapshot; without:
  stop only. A stack that was not running is not stopped, gets one
  snapshot (with a repository) and the new file, and stays stopped;
- the file is replaced atomically (temporary file in the stack directory,
  same mode and owner, fsync, rename);
- started are the services that ran before and are still defined, plus
  services the new file adds or that are defined but have no container yet
  (added while the stack was stopped); a stopped container stays stopped;
  waited for (running/healthy, 5 min) are the
  previously running ones (new services may be one-shot tasks). Containers
  of removed services (also a running one: it was stopped with the stack)
  stay stopped as orphans and are not started (logged; the host cleanup
  removes them, or `docker compose up --remove-orphans`);
- if they do not come up: rollback with a repository (restore of the final
  snapshot, which brings back the old compose file, with pinned images), no
  rollback without one.

## Snapshot cache

Listing snapshots on a remote repository can be slow, so the manager keeps a
cache in the `snapshots` table (id, short id, time, host, paths, tags, and
the backup summary restic >= 0.17 records in every snapshot).

- Refresh = `restic snapshots --json` against the stack's repository. It is a
  read-only operation, so the manager runs it directly (decrypting the repo
  credentials in memory and passing them to the child process via env), not
  through a worker container.
- Triggers: manual "Refresh" button, automatically after each successful
  backup job, and when opening the restore dialog if the cache is older than a
  configurable age. A background refresh runs at low frequency.
- Snapshots that disappeared on the server (the repo owner pruned) are marked
  `missing` on refresh and cannot be selected.
- The cache is only a convenience for selection. The worker re-verifies the
  selected snapshot against the repository before restoring.
- Show the cache age in the UI ("last refreshed 3 min ago").
- A repository may be shared by several stacks and hosts. The cache holds
  everything restic lists, but the UI (snapshot list, counts, restore) only
  uses the stack's own snapshots from this host: exactly one path whose
  last component is the stack name (restic records the backed-up path in
  every snapshot, so no extra tag is needed and older snapshots qualify
  too) and `hostname` = `STACKBELAY_HOSTNAME` (`store.StackSnapshots`). The
  page says how many other snapshots were hidden. The worker checks the
  path again before stopping anything.
- Sizes and durations (issue #9), from the summary: the Backups tab shows
  per snapshot what it **added** to the repository (`data_added_packed`,
  after deduplication and compression; the page says it is not the stack
  size: a final snapshot after a pre-stop snapshot adds little) and how
  long the backup took (`backup_end - backup_start`); the size backed up
  (`total_bytes_processed`, files) is the tooltip. Above the list:
  "Stack size at the last backup" from the newest snapshot with a
  summary. Snapshots without summary (restic < 0.17) show "–".
- **Pre-stop snapshots** (issue #35) are not listed by default
  (`snapshotRows`): every backup takes one (issue #34), and they are not
  consistent. Each listed snapshot shows its own size added and duration
  plus those of the pre-stop snapshots directly before it (usually the
  one of the same backup; more if earlier backups failed after theirs),
  so a row says what the backup cost; the duration's tooltip shows the
  final snapshot's own duration as the time the stack was stopped.
  Pre-stop snapshots newer than the newest other snapshot (a backup that
  failed or still runs) are counted in a hint. "Show pre-stop snapshots"
  (`?prestop=1`, advanced) lists every snapshot with its own values and a
  warning. Restoring a pre-stop snapshot is admin-only (link, form and
  action); its restore page warns that it may not be consistent.
- **Size on disk** ("Calculate" next to it, view right; also before the
  first backup, to estimate it): the manager walks the stack directory
  read-only (`measureDir`: regular files, apparent size, symlinks not
  followed, unreadable entries counted and named) and keeps the result in
  memory with its time and duration. At most one calculation per stack and
  two in total at a time, not again within 30 s, at most 5 minutes; a
  client that gives up does not stop it (the result shows on the next
  visit). Audited as `stack.size`.
- Job durations (`finished_at - started_at`, "running for …" while a job
  runs) in the job lists (Status tab, admin Jobs and Host tabs) and on the
  job page; no schema change.

## Web UI

Server-rendered `html/template` pages (`internal/web/templates`), one file
per page plus `base.html` (layout) and `partials.html` (shared snippets).
HTMX is used for small in-page updates; every HTMX form is also a normal
form with `method`/`action`, so it works without JavaScript. An HTMX request
(header `HX-Request: true`) gets an HTML fragment instead of a redirect.

Actions that are not possible in the current state (no repository yet, jobs
unavailable, nothing to initialize, a snapshot removed from the repository)
are shown **disabled** with a short reason, not hidden, so pages keep a
stable layout and users see what exists. Only what a user has no
permission for (admin tabs, backup/restore controls without the right) is
hidden.

Feedback while the server works (issue #7): `static/forms.js` (loaded on
every page) marks the submit button of a plain form busy when it is
submitted: a spinner (`button.busy`), disabled against a second click, and
the form's `data-busy` text if it has one ("Saving and checking access…",
"Initializing…", "Validating…"). HTMX forms (update check, snapshot
refresh) get the same spinner from the `htmx-request` class HTMX sets
during the request, and show their `.busy-text` ("asking the
registries…"). The Logs tab says "connecting…" and "waiting for the first
lines…" until the first line arrives. The same script asks the
`data-confirm` questions. Without JavaScript forms work unchanged.

All times are shown in the manager's time zone (`TZ`, `Server.Location`):
the `when` template function (job, snapshot and schedule times, with the
zone abbreviation, e.g. `2026-11-13 04:00 CET`), job log and container log
lines (the Logs tab names the zone), image dates of the update check. The
worker logs and restic report UTC; only the display converts. Internal
values stay UTC (database, event ids of the log streams).

Light and dark mode follow the browser/OS setting (`prefers-color-scheme`,
no switch in the UI). All colors in `static/style.css` are CSS variables
with a light and a dark set; the job log is a dark panel in both modes.

| Route | Who | |
|---|---|---|
| `GET /stacks/{name}/state` | view right | State cell of the stack list (HTMX fragment), see "Stack list" |
| `GET /` | logged in | stack list: the stacks the user may view (admins: all), see "Stack list" |
| `GET /stacks/{name}` | view right | "Status" tab, the home of a stack: containers with state and image, recent jobs, "Back up now" (backup right), see "Status tab" |
| `GET /stacks/{name}/status/live` | view right | the self-refreshing part of the Status tab (HTMX fragment, polled every 10 s) |
| `GET /stacks/{name}/backups` | view right | "Backups" tab: repository location, cached snapshots (same for every user) |
| `POST /stacks/{name}/backup` | backup right | queue a backup |
| `POST /stacks/{name}/up`, `/start`, `/restart`, `/stop`, `/down` | operate right | queue an up, start, restart, stop or down without backup, see "Stop, start and restart" (no stop or down for the manager's own stack) |
| `GET /stacks/{name}/logs` | logs right | "Logs" tab: container checkboxes and live log panel |
| `GET /stacks/{name}/logs/events` | logs right | live container logs (SSE), see "Container logs" |
| `GET /logs` | logs right on the chosen stacks | global log view: choose up to 4 stacks (`?stack=a&stack=b`), see "Global log view" |
| `GET /logs/events` | logs right on every `?stack=` | merged live container logs of those stacks (SSE) |
| `GET /stacks/{name}/update` | update right | update page (no tab, linked from Status): last check, unpin, update form; without repository it requires confirming "no backup, no rollback" |
| `POST /stacks/{name}/update/check` | update right | check for newer images (registry query, no pull), see "Update check"; HTMX: returns the Status tab's fragment |
| `POST /stacks/{name}/update` | update right | queue an update, see "Update pipeline" |
| `GET /stacks/{name}/restore?snapshot=<id>` | restore right | confirmation page with options |
| `POST /stacks/{name}/restore` | restore right | queue a restore of a cached, not removed snapshot (always with safety snapshot) |
| `GET /jobs/{id}` | view right on the job's stack | job details and log; followed live by `static/joblog.js` while not final |
| `GET /jobs/{id}/events` | view right on the job's stack | live stream of the job (SSE), see "Live logs" |
| `POST /jobs/{id}/cancel` | backup/restore/update/operate right (by job type; compose-apply: admin) | cancel a queued or running job |
| `POST /stacks/{name}/size` | view right | calculate the stack directory's size on disk (read-only walk in the manager; HTMX: returns the size section) |
| `POST /stacks/{name}/snapshots/refresh` | view right | refresh the snapshot cache (read-only restic; at most once per 10 s per stack; HTMX: returns the snapshot section) |
| `GET /stacks/{name}/shell?container=<id>` | admin, opt-in | shell page (no tab, linked per container from Status): terminal of a running container; without `container` a redirect to Status |
| `GET /stacks/{name}/shell/ws` | admin, opt-in | WebSocket of a shell session, see "Container shell" |
| `GET /stacks/{name}/compose` | admin | "Compose" tab: editor for the main compose file |
| `POST /stacks/{name}/compose` | admin | validate and queue a compose-apply job, see "Compose editor" |
| `GET /stacks/{name}/settings` | admin | "Settings" tab: repository, schedules with next run, enable/disable, per-user permissions |
| `POST /stacks/{name}/repository` | admin | create/update the repository, then refresh the snapshot cache as an access check |
| `POST /stacks/{name}/repository/init` | admin | `restic init` at the stored location (offered only when no repository exists there) |
| `POST /stacks/{name}/permissions` | admin | set one user's backup/restore right (HTMX: returns the table row) |
| `POST /stacks/{name}/source-host` | admin | set the host whose snapshots the stack lists and restores, see "Restore from another host" |
| `POST /stacks/{name}/schedules/enabled` | admin | enable/disable all schedules of the stack |
| `POST /stacks/{name}/schedules` | admin | add a schedule (cron expression, job type); at most 20 per stack |
| `POST /stacks/{name}/schedules/{id}/enabled` | admin | enable/disable one schedule |
| `POST /stacks/{name}/schedules/{id}/delete` | admin | delete one schedule |
| `GET/POST /admin/users` | admin | list users, create a user with a generated one-time password |
| `POST /admin/users/{id}` | admin | set admin/disabled flags (not on one's own account) |
| `POST /admin/users/{id}/password` | admin | generate a new password (must be changed, sessions end) |
| `GET /admin/jobs` | admin | "Jobs" admin tab: running and queued jobs of all stacks, recent jobs |
| `GET /admin/jobs/live` | admin | HTMX fragment of the Jobs admin tab, polled every 5 s |
| `GET /admin/host` | admin | "Host" admin tab: host cleanup, recent cleanups, host schedules; `?preview=1` lists what a cleanup would remove now |
| `POST /admin/host/cleanup` | admin | queue a host cleanup job |
| `POST /admin/host/schedules` | admin | add a cleanup schedule (cron expression) |
| `POST /admin/host/schedules/{id}/enabled` | admin | enable/disable a host schedule |
| `POST /admin/host/schedules/{id}/delete` | admin | delete a host schedule |
| `GET/POST /admin/import` | admin | "Import stack": list the stacks in a repository (read-only restic), see "Restore from another host" |
| `POST /admin/import/confirm` | admin | create the chosen stack record with that repository |
| `GET /admin/audit` | admin | "Audit log" admin tab: 100 entries per page, newest first; `?before=<id>` for older ones |

Repository form: the location may be a `rest:` URL with `user:password`,
which is split off into `RESTIC_REST_USERNAME`/`RESTIC_REST_PASSWORD`.
An empty password field keeps the stored password; backend variables are
entered as `KEY=value` lines and replace all stored variables (empty =
keep). Secret values are never rendered back into the page, only the names
of stored variables. After saving, a snapshot cache refresh checks access;
if it fails the repository stays saved and the restic error is shown.
If the check reports that no repository exists at the location (restic
exit code 10, `restic.IsNoRepository`), the Settings tab offers an
**Initialize repository** button (`POST /stacks/{name}/repository/init`,
admin, audited as `repository.init`). The manager runs `restic init`
directly with the stored password; restic refuses if a repository already
exists, so nothing is overwritten. This works with an append-only
rest-server and makes it easy to start a fresh repository e.g. per year,
keeping repositories and their indexes small.

### Stack list

The start page (`GET /`, decided in issues #23 and #37) lists the stacks
the user may view. Times are relative ("45 s", "12 min", "4 h", "25 d",
"1 y", `formatAge`; "in 5 h" for the future) with the full timestamp in the
tooltip; job states are dots with a symbol, not color alone (✓ succeeded,
✗ failed, pulsing ● running, … queued, ○ cancelled; tooltip and
`aria-label` carry the text). Columns, in two groups:

- **Stack** and **State** (see below).
- **Backup**: *Scheduling* (next run of the stack's enabled backup
  schedules, cron expression in the tooltip; `–` without schedule, `off`
  when all are disabled), *Repository* (✓ or `–`), *Last* (the newest
  consistent snapshot, with a link to the job that made it: a final
  snapshot of a backup, an update or a compose change, taken with the
  stack stopped; jobs that failed count only if they were rolled back to
  it, so the incomplete snapshot of a failed backup does not. If a newer
  backup job failed or still runs, the cell shows that job and "(ok X
  ago)" for the last consistent snapshot, or "never succeeded").
- **Update**: *Scheduling* (like backups), *Last check* (the newest update
  job in any outcome, or the last manual registry check of the update
  page if that is newer; it lives in memory and is lost on a restart;
  "update available" links to the update page when that check found newer
  images or pinned versions), *Last update* (the newest update that
  changed an image, `result.images_changed`, see "Update pipeline"; a compose
  change through the editor that replaced an image counts too).

"Refresh" reloads the rows (HTMX, `hx-select`), which reloads the State
cells and recomputes the times. On narrow screens the table scrolls
horizontally.

Stacks whose directory **vanished** (`present = 0`, `seen_present = 1`) are
hidden (issue #27); "Show stacks without directory (N)" lists them again
(`/?all=1`, bookmarkable, works without JavaScript), and a list that is
empty because of it says how many are hidden. A stack imported from
another host that was never restored here (`seen_present = 0`) is always
listed, with the badge `not restored yet` instead of `missing` (also in
the stack header). Other lists (global log view, Host tab, permissions)
are unchanged.

The State cell loads lazily (HTMX, `GET /stacks/{name}/state`, again on
"Refresh"), so the page does not wait for docker:

| State | Meaning |
|---|---|
| missing | the stack directory is gone (rendered directly, no request) |
| busy: `<type>` | a job of the stack runs; links to it (no docker call) |
| not created | the stack has no containers |
| started | every service runs (and is healthy, if it has a healthcheck) |
| stopped | no container runs |
| degraded | some but not all services run, or a container is unhealthy or restarting; containers that exited with code 0 while others run (one-shot services) are ignored |
| legacy (compose v1) | containers created by docker-compose v1 (`docker.ErrLegacyCompose`, see "Interfaces") |
| unknown | reading the state failed (error in the tooltip) |

The manager reads it with `docker.CLI.StackOverview` (`compose config
--format json`, the project owner check, `compose ps --all`). Results are
cached per stack for 5 seconds; concurrent requests for the same stack
share one load, and at most 4 loads run at a time (`stateCache`).

The page header of a stack shows the `missing` and `restore incomplete`
badges.

Stack pages share a header with tabs (`stack-header` in `partials.html`).
The tab list is built per user in `newStackHeader`; a tab is only shown if
the user may open it, and its handler checks the permission again. Tabs:
"Status" (view right, the default), "Backups" (view right), "Logs" (logs
right), "Compose" and "Settings" (admin).

### Status tab

The home of a stack (`GET /stacks/{name}`, decided in issue #11). It shows
the stack's containers (`compose ps --all` via `docker.CLI.StackContainers`,
after the project owner check): name, service, state with health and
docker's status text, image and, after an update check, its result.
Below them the recent jobs of the stack. Users with the backup right get
"Back up now" there, users with the update right "Check for updates" and
"Update…" (the update page), users with the operate right "Restart",
"Stop" and "Start" (Stop and Restart ask for confirmation,
`static/forms.js`); every action checks its own right. Each
container row links to the Logs tab with only that container selected
(logs right) and, for admins with `STACKBELAY_ENABLE_SHELL`, to a shell in
it (shown disabled while the container is not running).

Containers and jobs are one fragment (`status-live`) that HTMX reloads
every 10 seconds from `GET /stacks/{name}/status/live`, so a running job
and the containers it stops and starts show up without reloading the page.
These polls carry the header `X-Stackvault-Poll` and do **not** extend the
session (`loadSession`): an open tab alone does not keep a login alive. An
HTMX request without a valid session gets `401` with `HX-Redirect: /login`,
so HTMX loads the login page instead of swapping it into the fragment.

### Admin pages

The header shows admins an "Admin" link to pages outside the stacks, with
their own tabs (`admin-tabs` in `partials.html`): "Users" (`/admin/users`),
"Jobs" (`/admin/jobs`; host jobs show "host" as stack), "Import stack"
(`/admin/import`, see "Restore from another host"), "Host"
(`/admin/host`, the host cleanup: preview, "Clean up now", recent cleanups,
schedules; see "Host cleanup") and "Audit log" (`/admin/audit`, read-only,
100 entries per page with user and stack names). Host jobs' pages are
admin-only too (404 for others). The Jobs tab lists the running and queued jobs
of all stacks (running first, each oldest first) and the 50 newest jobs;
like the Status tab it is one fragment that HTMX reloads every 5 seconds
(`GET /admin/jobs/live`, with `X-Stackvault-Poll`). The account link in the
header reads "Profile" (the username is its tooltip).

Stacks a user may not view answer 404 (not 403), so stack names are not
revealed. `{name}` must pass `stacks.ValidName` and exist in the database
(i.e. was discovered).

Static assets are embedded from `internal/web/static`. Templates reference
them with `{{asset "name"}}`, which adds the file's content version
(`/static/style.css?v=<12 hex of its SHA-256>`, computed once at startup,
`assetVersions`; issue #20). A request with the current version may be
cached for a year (`immutable`); any other gets `Cache-Control: no-cache`
and an `ETag` (the version), so a browser never keeps an old script or
style sheet after an upgrade. Vendored assets:
`htmx-2.0.11.min.js` (0BSD, `htmx-LICENSE.txt`; from the npm package
`htmx.org@2.0.11`, tarball integrity checked against the registry). To
update, replace the file, keep the version in the file name (cache busting)
and update `base.html`. HTMX is configured in `base.html` (`htmx-config`
meta) to work with the strict CSP: no injected indicator styles, no
`eval`, no scripts from responses, requests to the own origin only.
`xterm-6.0.0.js`, `xterm-6.0.0.css` and `xterm-addon-fit-0.11.0.js` (MIT,
`xterm-LICENSE.txt`, `xterm-addon-fit-LICENSE.txt`; the prebuilt files of
the npm packages `@xterm/xterm@6.0.0` and `@xterm/addon-fit@0.11.0`,
tarball integrity checked against the registry) are loaded only by the
shell page.

## Job progress

While a job runs, the job page shows what it is doing (issue #5): the
current phase ("pre-stop snapshot", "stopping stack", "final snapshot",
"restoring", "recreating services", "waiting until healthy", ...) and,
while restic backs up or restores, a progress bar with percent, bytes and
files done, average speed and (backups only, restic reports it) the
remaining time. During the rollback of an update or compose apply every
phase is prefixed "rollback: ". While the stack stops and while the job
waits until services are healthy, the services still pending are shown
("waiting for: app, db"; issue #8). A queued job says what it waits for:
the running (or earlier queued) job of its stack, or a free slot when
`STACKBELAY_MAX_JOBS` jobs run, and how many jobs are queued before it
(`store.QueueState`, the same rules as `ClaimNextJob`). A running job that
waits for its downtime slot shows the phase "waiting for a downtime slot".

- The pipeline reports phases to `Pipeline.Progress` (`jobs.ProgressReporter`;
  nil reports nothing). `restic.CLI.Progress` receives restic's `--json`
  status lines of `backup` and `restore` (restic >= 0.17 for restore;
  skipped bytes of a restore count as done). With it set,
  restic runs with `RESTIC_PROGRESS_FPS=1`, so it prints one status line
  per second instead of up to 60; the first comes after one second, so
  transfers shorter than that show no numbers.
- `docker.CLI.Pending` receives the services still pending: `WaitHealthy`
  reports them on every poll; `Stop` starts a goroutine that runs
  `compose ps` every 2 s while `compose stop` waits for the containers
  (each up to its stop timeout) and reports the services still running.
  The worker turns them into the detail of the current phase.
- Image pulls of an update (issue #6): with `docker.CLI.Pulling` set,
  `Pull` runs `docker compose --progress json pull` and reads its JSON
  lines (stderr) while it runs: per image and layer, the bytes downloaded
  of the layers whose size is known. It reports at most once per second
  (at once when an image is done): the bytes as transfer (bar and speed;
  no bar while no size is known) and a summary per image as detail
  ("nginx:1.27 12.3 MiB of 20.1 MiB, alpine:3.20 done"). The single
  `docker pull` of a pinned restore (`EnsureImage`) only shows its phase
  ("getting image ..."): its plain-text output has no stable format.
- In the worker all of them feed `jobs.FileProgress`, which writes
  `progress.json` into the job directory: atomically (temporary file and
  rename, owner like the other job files), phases and details at once,
  transfer numbers at most once per second (the first of a phase at once).
  Write errors are ignored: progress never fails a job.
- The manager reads it (`jobs.ReadProgress`) only for running jobs and
  renders it in the job details section, which the live stream already
  re-sends when it changes (`details` event, checked every 500 ms). The bar
  is a native `<progress>` element (no inline styles, CSP unchanged).
- restic's own messages (issue #42): `restic.CLI` logs every stderr line
  of restic while it runs, redacted like the error tail: retries of a
  failing repository server, lock messages, warnings, and the per-file
  `error` lines of `--json` (with item, during and error). The fatal
  `exit_error` is not logged because the returned error carries it. At
  most 200 lines per restic command are logged. When restic has printed
  nothing on stdout for a minute (`CLI.SilenceInterval`), "restic is
  still running, no output" is logged with the elapsed time, and again
  every minute. Backups and restores print a status line every second,
  so this only appears when restic waits, e.g. for a slow repository
  server. In the worker these lines go to the job log, in the manager
  (snapshot refresh) to its own log.

## Live logs

The worker appends to `/job/log.txt` as JSON lines (`log/slog`; secrets
redacted before writing).

Job directories (log, status, spec, progress) of jobs that finished more
than `STACKBELAY_JOB_LOG_RETENTION` ago (default `2160h` = 90 days; `0`
keeps them) are deleted by the dispatcher, at start and every 6 hours
(`CleanupJobDirs`, issue #16). One rule for every final state; queued and
running jobs are never touched. The job rows stay, so job lists, the stack
list and results keep their history; the job page says that the log was
deleted.

- `GET /jobs/{id}/events?offset=N` streams a job as Server-Sent Events
  (`text/event-stream`): `line` (one log line as server-rendered HTML; event
  id = byte offset in `log.txt` after the line), `details` (the job details
  section as HTML, sent when state, times or results change), `done` (job
  final and log sent completely; the stream ends). Only complete lines are
  sent; a line the worker is still writing waits for the next round
  (every 500 ms). A comment every 15 s keeps idle connections open.
- The job page renders the end of the log (up to 512 KiB) and its offset;
  `static/joblog.js` (plain script, no build step) continues the stream
  from there. It does not use the HTMX SSE extension: the browser's own
  reconnect gives up on HTTP errors (e.g. 502 from a reverse proxy while the
  manager is down), and a resumed stream must start exactly behind the last
  received line. The script reconnects with `?offset=<last event id>` and
  backoff (1 s doubling up to 15 s) and shows "reconnecting…".
- Self-backup / self-restore: the manager is stopped while the job runs; its
  open streams end immediately on SIGTERM (`web.Server.Stopping`) instead of
  delaying the shutdown. The browser keeps reconnecting until the manager is
  back, then receives the missed lines and the final details.
- Viewing the stream needs the view right on the job's stack (like the job
  page). Log files are kept per job.

## Container logs

The "Logs" tab (`can_logs`, admins always) shows the live output of all
containers of a stack, mixed in the order `docker compose logs` prints
them. One checkbox per container shows or hides its lines (opened with
`?container=<name>`, the Logs link on the Status tab, only that container
starts checked; an unknown name selects all); filtering
happens in the browser only (`static/stacklog.js`). Each container has
its own color (8 classes `cc0`..`cc7` in `style.css`, assigned in the
order of the checkboxes): its name and a bar at the left of its lines, and
a swatch next to its checkbox as legend. `compose logs` sends the history
container by container and mixes only the lines it follows live, so the
script inserts every line in time order: the event id and the line's
`data-t` are its time in UTC with always nine fraction digits
(`logLineIDFormat`), which orders correctly as a string. A reconnect
resumes after the newest line seen. The stream always
carries all containers. "Wrap lines" (on by default, remembered per
browser in `localStorage`) can be turned off for horizontal scrolling.
**Colors** (issue #29, on by default, remembered the same way): the ANSI
colors applications write into their output are shown. `ansiHTML`
converts the SGR sequences of each line on the server into `<span>`
elements with fixed classes (16 foreground and background colors, bold,
dim, italic, underline; 256-color and true-color codes map to the nearest
of the 16), with every piece of text HTML-escaped and no inline style
(CSP). Every other escape or control sequence (cursor movement, titles,
hyperlinks) is removed; after a carriage return only the last segment
of a line is kept (progress bars); open spans are closed at the end of
each line. The palette is the dark log panel's (both themes); turning
colors off shows plain text. Compose's own prefix colors stay off
(`--no-color`); the job log is not affected.
"Full screen" makes the log cover the browser window (not the browser's
own full-screen mode) with only an "Exit full screen" button; Esc exits
too.

- The manager runs the logs itself (no worker): `docker compose
  --project-directory <stack dir> logs --follow --timestamps --no-color
  --tail 500` (`docker.CLI.FollowLogs`, fixed argument list). The stack
  directory comes from discovery (`stacks.Lookup`), never from the URL.
  Before reading, the compose project must belong to that directory (same
  check as the pipelines), so a project name clash cannot show another
  project's logs. The checkbox list comes from `compose ps --all`, with
  names as compose prefixes log lines (`app-1`: container name without
  `<project>-`; a custom `container_name` as is).
- `GET /stacks/{name}/logs/events` streams Server-Sent Events: `line`
  (one line as escaped HTML with `data-c` = container; event id = the
  line's timestamp, RFC 3339), `end` (compose ended on its own) and
  `failed` (error text; not named `error`, which would also fire
  `EventSource.onerror`). A comment every 15 s keeps idle connections
  open. Lines are cut at 16 KiB; the browser keeps the last 5000.
- The compose process ends with the request (client gone) or when the
  manager shuts down. Current Compose versions keep following a stopped
  stack, so a backup's stop/start does not end the view. On a lost
  connection the script reconnects with `?since=<last id>` (resumes 1 ns
  after that line) and backoff up to 30 s, after checking that the page is
  still accessible.
- Limits: 4 open streams per user, 20 in total (each is a compose
  process); more answer 429 and the browser retries with backoff.
- Logs are **not** redacted: applications may log secrets, hence the
  separate right. A new stream (without `since`) is audited as
  `stack.logs`; reconnects are not.

### Global log view

Decided in issue #21. `GET /logs` (header link "Logs", shown to users with
the logs right on at least one stack) follows the container logs of
several stacks in one view, with the same panel and script as the Logs tab
(`log-panel` in `partials.html`, `static/stacklog.js`).

- The page lists the stacks with the logs right (missing stack
  directories disabled); a plain GET form puts the choice into the URL
  (`?stack=a&stack=b`), so the view can be bookmarked. Every name must be a
  discovered stack with the logs right, else 404 like the stack pages (also
  for the stream); more than 4 answer 400.
- `GET /logs/events?stack=…` runs one `compose logs` per stack (the same
  command, with `--tail 100`) and merges their lines into one SSE stream
  (`streamLogs`, shared with the Logs tab). Lines carry `data-c` =
  `<stack>/<container>`, so the container filter and colors work across
  stacks; compose's own lines show the stack name. When the logs of one
  stack end or fail (`failed` names the stack), the whole stream ends and
  the browser reconnects with `?since=` for all stacks.
- Each stack counts as one stream against the limits (4 per user, 20 in
  total); a view reserves all of them at once or gets 429.
- Rights are checked on the page and on every (re)connect. A new stream is
  audited as `stack.logs` per stack (detail `global log view`).

## Container shell

Opt-in (`STACKBELAY_ENABLE_SHELL=true`, default off) and admin-only: the
"Shell" link of a container on the Status tab opens an interactive shell
in it. Without the setting the links and the routes do not exist (an
exception to "disabled, not hidden": the feature is meant to be invisible
where it is not wanted).

- `GET /stacks/{name}/shell?container=<id>` shows the terminal (xterm.js,
  `static/shell.js`) of a running container of the stack's container list
  (`docker.CLI.StackContainers`: `compose ps --all` after the project owner
  check). Without `container` it redirects to the Status tab.
- `GET /stacks/{name}/shell/ws?container=<id>&csrf=<token>&cols=&rows=` is
  the WebSocket (`github.com/coder/websocket`). It requires an admin
  session, the session's CSRF token (the upgrade is a GET, so the token is
  a query parameter) and a same-origin request (`websocket.Accept` rejects
  other origins). The container id must belong to the stack's container
  list and be running.
- The manager runs `docker exec --interactive --tty --env
  TERM=xterm-256color <id> /bin/bash` (or `/bin/sh` if `test -x
  /bin/bash` fails) in a pseudo-terminal (`github.com/creack/pty`), as the
  container's default user. Binary WebSocket messages carry terminal data
  in both directions; a text message `{"type":"resize","cols":..,"rows":..}`
  resizes the terminal (the docker CLI passes it on).
- The session ends when the shell exits, the browser closes it, the manager
  shuts down, or at the next check (every 30 s) after the login is no
  longer valid (logout, expiry, disabled, admin flag removed). The server
  then sends a normal close with the reason. There is no reconnect: a new
  shell is a new session.
- Limits: 2 shells per admin, 10 in total. `shell.start` and `shell.end`
  (container, duration, reason) are audited; keystrokes and output are not
  recorded.
- CSP: the shell page alone allows inline styles (`style-src 'self'
  'unsafe-inline'`), because xterm.js adds `<style>` elements; scripts stay
  same-origin only.

## Host cleanup

Decided in issue #19. A host job (type `cleanup`, no stack; see "Host
jobs" in data-model.md: it runs alone, nothing else runs beside it) that
removes what updates and compose changes leave behind. It never prunes
(`docker system/image/container prune` is not used), never touches volumes
and never stops anything. The rules (`jobs.PlanOrphans`,
`jobs.PlanImages`) are shared by the worker and the manager's preview; the
job recomputes them itself.

1. **Orphan containers**: for every discovered stack, after the project
   owner check, the containers of its compose project whose
   `com.docker.compose.service` is no longer in the compose file
   (`docker.CLI.StackOrphans`). Stopped ones are removed with `docker
   container rm -- <id>` (no `--force`, so docker refuses a running one);
   running ones are only logged. A stack whose containers cannot be
   listed (foreign project, docker-compose v1) is skipped with a warning.
2. **Dangling images**, listed after step 1 (an orphan's image may have
   become unused): untagged images that no container, running or stopped,
   uses (`docker.CLI.DanglingImages`). Docker records no "dangling since"
   time, so an image is kept if
   - an update or compose apply replaced it within the minimum age
     (`result.replaced_images` of the jobs; the manager passes them as
     `keep_images` in the spec), or
   - it was last pulled or tagged on this host (`Metadata.LastTagTime`,
     else the build time) less than the minimum age ago
     (`image_min_age_hours`, default 7 days).

   Others are removed with `docker image rm -- <id>` (no `--force`).
   A rollback or pinned restore that needs a removed version pulls it again
   by digest (`EnsureImage`).

   The minimum age is `STACKBELAY_CLEANUP_IMAGE_MIN_AGE`; when the job
   starts, the dispatcher puts it into the spec together with the
   `replaced_images` of the jobs finished within it (at most 500; beyond
   that, images are protected only by their pull/tag time).

**Preview** (admin "Host" tab, `?preview=1`): the manager lists the same
read-only (`jobs.ListCleanup` with the same minimum age and the same
replaced images) and shows what would be removed or kept and why, with
the sizes. It is computed only on request (a button, plain GET form), not
on every visit. The job does not use it: it lists again when it runs.

A removal that fails is logged and the cleanup continues; the job then
ends `failed`. The result records `removed_containers`, `removed_images`
and `freed_bytes` (the sizes of the removed images; shared layers are
counted per image, so the space freed on disk may be smaller).

## Scheduler

`internal/scheduler`, using `robfig/cron/v3` in the manager. A schedule only
creates a `queued` job (trigger `schedule`, no requesting user); execution
always goes through the same queue, per-stack lock and worker path as manual
runs. Each schedule is enabled or disabled on its own; "Enable all" and
"Disable all" on the Settings tab switch all schedules of a stack (issue
#37: there is no stack-level enabled flag). A schedule has a job type
(Settings tab, admins):

- **backup** (with pre-stop snapshot, like every backup);
- **update** (like every update, it never stops or backs up a stack when
  no running service gets a new image or is pinned).
  It fails if the stack is not running. On a
  stack without repository, creating an update schedule requires the same
  confirmation as a manual update ("no backup and no rollback": a checkbox
  checked by the server, and a dialog, `static/forms.js` with
  `data-confirm-when="job_type=update"`). The schedule stores it
  (`schedules.no_backup`) and queues its updates with
  `JobOptions.NoBackup`; snapshot and rollback options are dropped. If the
  stack has a repository when the job starts, it is backed up as usual (see
  "Update pipeline"). An update schedule without that confirmation (e.g.
  created while the stack had a repository that was removed later) fails.
- **cleanup**, a host schedule (no stack, admin "Host" tab, no options):
  queues a host cleanup (see "Host cleanup").

- Expressions: the standard 5 fields (minute hour day-of-month month
  day-of-week) or descriptors (`@daily`, `@weekly`, `@every 6h`, ...).
  `scheduler.Parse` validates them before saving: no seconds field, no
  per-schedule `CRON_TZ=`/`TZ=` prefix, `@every` at least 1 minute, at most
  100 characters, and the expression must match some date. Expressions are
  stored normalized (single spaces).
- Time zone: the manager's zone (`TZ`, `config.Manager.TimeZone`), passed
  explicitly to cron (`cron.WithLocation`) and to the next-run calculation
  of the Settings tab. The UI shows all times in the same zone and names it
  next to the schedule form.
- Loading: `Scheduler.Run` loads all enabled schedules at start; the web
  handlers call `Reload` after every change, which replaces all cron entries.
- Firing (`Scheduler.Fire`): re-reads the schedule and its stack, skips it if
  either was disabled (or the schedule deleted) since the last reload, then
  submits the job. If a job of the same type is already queued for the
  stack, the run is skipped (logged); a running job does not block queueing
  the next one.
  Audit entry `job.create` without user. A stack without repository or
  directory is not special-cased: the job is queued and fails visibly
  (the Settings tab warns about schedules without a repository).
- Missed runs (manager down, e.g. during its own backup) are not replayed.
- Disabling a stack skips all its schedules; manual jobs stay possible.
- The scheduler runs only if the dispatcher could be created; otherwise
  schedules can still be edited and take effect after a restart.

## Stack discovery

Every direct subdirectory of `STACKS_ROOT` containing a compose file is a
stack. Accepted names are those `docker compose` looks for, in its order:
`compose.yaml`, `compose.yml`, `docker-compose.yml`, `docker-compose.yaml`;
the first one that exists as a regular file (not a symlink) is the stack's
main file (`stacks.MainFileNames()`). The directory name is the stack ID. Names ending
in `.restore-tmp` or containing `.old-` are reserved (the staging and
previous-version directories of restores up to StackVault 0.4) and are
never discovered. Discovery is the source of stack names and paths; the
only other source is an admin's "Import stack" from a repository's snapshot
paths (see "Restore from another host").

## Interfaces (for testing)

The pipelines in `internal/jobs` depend only on two small interfaces; the
real implementations shell out to the CLIs (`docker.CLI`, `restic.CLI`),
tests use fakes.

```go
// internal/docker
type Client interface {
    Services(ctx context.Context, projectDir string) ([]ServiceState, error)
    Stop(ctx context.Context, projectDir string) error
    Start(ctx context.Context, projectDir string, services []string) error // compose start (after backup)
    Up(ctx context.Context, projectDir string, services []string) error    // compose up -d --force-recreate (after restore; nil = all)
    WaitHealthy(ctx context.Context, projectDir string, services []string) error
}

// internal/restic
type Runner interface {
    CheckRepo(ctx context.Context, repo Repo) error // restic cat config
    Backup(ctx context.Context, repo Repo, path string, opts BackupOptions) (string, error)
    Snapshots(ctx context.Context, repo Repo, ids ...string) ([]Snapshot, error)
    Restore(ctx context.Context, repo Repo, snapshotID, subpath, target string) error // <id>:<subpath>
    Node(ctx context.Context, repo Repo, snapshotID, path string) (Node, error)        // restic ls, metadata of path
}
```

Compose commands run with the stack directory as working and project
directory and without `-f`, so `compose.override.yml` is honoured exactly as
when a user runs `docker compose` there. The compose project name is therefore
the directory name (or `name:` in `compose.yml`). Before touching a stack,
`docker.CLI` checks that every existing container of that project was
created from this stack's directory (label
`com.docker.compose.project.working_dir`); otherwise the job fails before
anything is stopped. This protects against a stack of the same name
elsewhere on the host or one started with `docker compose -p`.
Containers created by the Python docker-compose v1 have no working
directory label but a `com.docker.compose.version` label `1.*`: they are
refused with `docker.ErrLegacyCompose` ("recreate them once with `docker
compose up -d`"; the stack list shows such a stack as "Legacy (compose
v1)"). The check also looks for them under the project name v1 before 1.21
derived (only letters and digits, e.g. `myapp` for `my-app`), because
Compose v2 would not see them and start a second copy of the stack next
to them. `restic restore <id>:<subpath>` requires restic >= 0.17.

## Worker CLI

```
worker run <job-dir>
worker backup  <stack>
worker restore [--no-start] [--pin-images] [--import] [--limit-download <KiB/s>] <stack> <snapshot-id>
worker update  [--no-backup] <stack>
worker stop|start|restart|up|down <stack>
worker cleanup [--image-min-age <duration>]
```

`run` is the manager's interface (see "Job handoff"); the other commands
are for manual use and tests. The repository settings
(`RESTIC_REPOSITORY`, `RESTIC_PASSWORD(_FILE)`) are optional in the worker's
configuration, but every job needs them except `update --no-backup`,
`stop`, `start`, `restart`, `up`, `down` and `cleanup`.

Flags go before positional arguments. Exit codes: 0 success, 1 job failed,
2 usage/configuration error. Logs are JSON lines (`log/slog`) on stderr.
The repository always comes from the environment:

| Variable | Meaning |
|---|---|
| `STACKS_ROOT` | absolute stacks root (required) |
| `STACKBELAY_HOSTNAME` | value for `restic --host` (defaults to `os.Hostname()`, which is random inside a container, so set it there) |
| `RESTIC_REPOSITORY` | repository (required) |
| `RESTIC_PASSWORD_FILE` / `RESTIC_PASSWORD` | one is required |
| backend variables (`AWS_*`, `B2_*`, `AZURE_*`, `GOOGLE_*`, `RESTIC_REST_*`) | passed to restic; values are redacted from output |
| `STACKBELAY_JOB_TIMEOUT` | hard job timeout, Go duration (at least `1m`), default `48h` |
| `STACKBELAY_DOWNTIME_TIMEOUT` | limit for stop -> final snapshot, Go duration (at least `1m`), default `1h` |
| `STACKBELAY_LOG_LEVEL` | `debug`, `info`, `warn`, `error` |

restic never inherits the full process environment: it gets an allow-list
(`PATH`, `HOME`, `TMPDIR`, proxy/CA settings, `RESTIC_CACHE_DIR`, ...), the
repository location and the repository's variables, which must be in
`restic.BackendEnvVars`. `RESTIC_PASSWORD_COMMAND` is intentionally not
supported (it would turn configuration into command execution).

End-to-end smoke test against the local Docker daemon and a temporary local
repository: `scripts/e2e-local.sh` (`KEEP=1` keeps the temp directory and
containers for inspection). `scripts/e2e-dispatch.sh` tests the manager
with real worker containers through the web UI (repository init, backup,
queueing, scheduled backup, restore, pinned restore); it builds the release
image from `deploy/Dockerfile`, as does `scripts/e2e-self.sh`.
`scripts/e2e-update.sh` runs the update job on the host: a successful
update, a failed one with automatic rollback, and a failed one without
backup (no rollback); and the compose-apply job: a valid change and one
with a missing image (rollback to the previous compose file).
`scripts/e2e-migrate.sh` moves a stack between two simulated hosts (two
stacks roots and host names, one repository): import without start,
refused second import, sync with safety snapshot, final
restore with start and pinned images, and backups under the new host's
name.
