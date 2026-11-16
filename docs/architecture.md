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
- Least privilege can be split later (manager without socket access, only a
  thin launcher with it).

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
| `STACKVAULT_HOSTNAME` | name of the Docker host, passed to workers for `restic --host` (required) |
| `STACKVAULT_SECRET_KEY` / `STACKVAULT_SECRET_KEY_FILE` | 32-byte key, base64 (`openssl rand -base64 32`); at most one may be set. Neither set: `<STACKVAULT_DATA_DIR>/secret.key`, created on a fresh install. See "Secret key" |
| `STACKVAULT_DATA_DIR` | database and job directories, default `/data` |
| `STACKVAULT_LISTEN` | listen address, default `:8080` |
| `STACKVAULT_SESSION_TTL` | session lifetime, default `12h` |
| `STACKVAULT_ALLOW_INSECURE_HTTP` | accept logins over plain HTTP (testing without TLS proxy), default `false` |
| `STACKVAULT_WORKER_IMAGE` | image for worker containers, default: the manager's own image |
| `STACKVAULT_MAX_JOBS` | jobs running at the same time across all stacks, default `1` |
| `STACKVAULT_ENABLE_SHELL` | admin-only container shells ("Shell" tab), default `false`; see "Container shell" |
| `STACKVAULT_LOG_LEVEL` | `debug`, `info`, `warn`, `error` |
| `TZ` | time zone of the cron schedules (e.g. `Europe/Berlin`), default UTC in most images; the zone database is built into the binary |

The database file is created with mode 0600. `/healthz` checks the database
and is meant for the container health check. Stacks are discovered at
startup and mirrored into the database.

### Secret key

The key encrypts repository credentials in the database (AES-256-GCM, see
`internal/secrets`). Sources, in this order:

1. `STACKVAULT_SECRET_KEY` (base64 in the variable) or
   `STACKVAULT_SECRET_KEY_FILE` (path of a file containing it). Setting
   both is a configuration error. If `<data>/secret.key` exists as well, it
   is ignored with a warning.
2. Neither set: `<STACKVAULT_DATA_DIR>/secret.key`.

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
`STACKVAULT_SECRET_KEY_FILE` with a file outside `STACKS_ROOT` (e.g.
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
snapshot ids, previous directory, error; see `jobs.Status`) when the job
starts and when it ends, and `log.txt` as JSON lines. Both log and error are
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
(`/data` is part of it). Restoring that stack would move the job files away
with the previous version and bring back an older database. So for a
restore of the stack that contains the jobs directory (and for an update
or compose apply with backup, whose rollback is such a restore), the spec
carries
`carry_over_jobs` (the jobs directory relative to the stack, e.g.
`data/jobs`): right after the swap, before the stack starts, the worker
copies it from the previous version into the restored stack, and at the
end writes its final status and complete log into that copy too. An
in-place restore excludes that directory from the restore instead (see
"In-place restore"); the job files never move. The
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
2. Claim queued jobs (oldest first, stack not locked, at most
   `STACKVAULT_MAX_JOBS` running in total), write `spec.json`, start the
   worker container. Start errors (no repository, `docker run` failed) fail
   the job and release the lock.

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
restarts the stack and reports `cancelled`.

Worker container template (fixed, nothing from user input):

```
docker run --detach --name stackvault-job-<id>
  --label stackvault.worker=true --label stackvault.job=<ID>
  --mount type=bind,source=$STACKS_ROOT,target=$STACKS_ROOT
  --mount type=bind,source=<host docker socket>,target=/var/run/docker.sock
  --mount <host jobs dir>/<ID> -> /job   (bind, or volume with volume-subpath)
  --mount type=volume,source=stackvault-restic-cache,target=/cache
  --env STACKS_ROOT --env STACKVAULT_HOSTNAME --env RESTIC_CACHE_DIR
  --env RESTIC_REPOSITORY --env RESTIC_PASSWORD --env <backend variables>
  <image> /app/worker run /job
```

The manager finds the host side of its mounts and its own image by
inspecting its own container (id from `/proc/self/mountinfo`).
`STACKS_ROOT` must be bind-mounted from the same path; `/data` may be a
bind mount or a named volume (Docker >= 26 for `volume-subpath`). Outside a
container (development) paths are used as they are and
`STACKVAULT_WORKER_IMAGE` is required. The docker CLI gets an allow-listed
environment (`PATH`, `HOME`, `DOCKER_*`) plus the job's credentials, which
`--env NAME` forwards by name.

## Backup pipeline

1. **Preflight**: stack exists in discovery, repo reachable (`restic cat config`),
   lock acquired.
2. **Record state**: for each service, record whether it is running, plus the
   image reference, the image ID and the **repo digest(s)**
   (`docker compose ps --format json`, `docker image inspect` ->
   `.Id` and `.RepoDigests`). Write `metadata.txt` into the stack folder (it is
   then part of the snapshot). See "metadata.txt format" below.
3. **Pre-stop snapshot (optional)**: stack still running, `restic backup`
   with tag `pre-stop`. Not guaranteed consistent; its purpose is to make the
   final run fast (parent snapshot, small delta). Failure here is non-fatal:
   log a warning and continue without a parent.
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

1. **Preflight**: permission re-checked by the manager, snapshot exists, lock.
2. **Safety snapshot**: back up the current state with tag `pre-restore`
   (stack is stopped first to keep it consistent; skip only if explicitly
   disabled by an admin). Skipped if the stack directory does not exist yet
   (see "Restore from another host").
3. **Stop** the stack (skipped if it does not exist yet).
4. **Restore to staging**: `restic restore <snap>:<snapshot path> --target <root>/<stack>.restore-tmp`
   on the same filesystem (so renames are atomic). The `<snap>:<subfolder>`
   syntax restores the *contents* of the snapshot's stack directory directly
   into the target, without recreating the absolute path
   (`<target>/home/docker/<stack>/...`). `<snapshot path>` is taken from the
   snapshot's `paths[0]`, so it works regardless of the source host's
   `STACKS_ROOT`. This form does not restore the metadata of the stack
   directory itself (restic creates the target with mode 0700), so owner,
   mode and mtime are read with `restic ls --json <snap> <snapshot path>`
   and applied to the staging directory before the swap.
5. **Swap**: rename `<stack>` to `<stack>.old-<ts>`, rename staging to `<stack>`
   (for a new stack: just rename staging to `<stack>`).
6. **Start** the stack (services that were running at snapshot time, taken from
   `metadata.txt`; fallback: all) with `docker compose up -d --force-recreate`.
   Recreating is required: containers must pick up the restored compose
   configuration, and Docker Desktop (WSL integration) pins bind mounts per
   container, so a plain `start` would still show the renamed old directory.
   Volumes are not removed. Image versions are chosen per the restore
   option "image versions" (see "Pinning image versions on restore").
7. **Verify** health. On failure, report; keep `.old-<ts>` for manual rollback.
   Cleanup of `.old-*` is a separate explicit action.

Self-restore works for the same reason as self-backup. Mount the parent
directory, never the stack folder itself, otherwise the swap would pull the
mount point away from under the worker.

### In-place restore

Restore option **method** (UI radio buttons, default "switch over"; job
option `in_place`, worker flag `--in-place`), for any user with the restore
right. Restoring into staging and swapping needs room for a complete second
copy and downloads the whole snapshot, which is slow and expensive for
large stacks (photo libraries, file sync). In place, steps 4 and 5 become:

4. `restic restore <snap>:<snapshot path> --target <root>/<stack> --delete
   --overwrite if-changed [--exclude /<carry_over_jobs>]` (restic >= 0.17).
   Files with the same size and modification time as in the snapshot count
   as unchanged and are neither read nor written; the others are verified
   and rewritten where their content differs; files that are not in the
   snapshot are deleted. The manager's jobs directory (self-restore) is
   excluded, which also protects it from `--delete`. Then the compose file
   check, the directory metadata and the image pinning run on the stack
   directory itself.
5. No swap and no `<stack>.old-<ts>` directory.

Failure handling differs once restic has started writing: the old state is
gone, and starting a half-restored stack could damage its data further.

- Failure before that (stop, safety snapshot): as with a swap, the stack is
  started again as it was.
- Failure during the restore or before the stack is completely restored
  (restic, directory metadata): the job fails, the stack **stays
  stopped**, and the error says how to get back: restore again, or restore
  the safety snapshot. Without a safety snapshot (admins only) there is no
  way back to the state before.
- Failure after a complete restore but before the start (e.g. a pinned
  image that cannot be pulled): the stack stays stopped with the restored
  files.
- Failure of the start: reported like any failed start.

The safety snapshot is therefore the only rollback of an in-place restore;
the UI says so next to the option.

Automatic restores (the rollback of an update or a compose apply) use the
swap unless the user chose "roll back in place" for that job (update form,
compose editor). Whether that choice should become a per-stack default is
still open, see "Rollbacks: in place or with swap" under "Planned
features".

## Pinning image versions on restore

Goal: after a restore, run the image versions that were running at backup
time, not whatever the tag (`:latest`, `:29`, ...) points to today.

Restore option **image versions**:

- `tags` (default): start as defined in `compose.yml`. Tags resolve to current
  images (may pull newer ones).
- `pinned`: use the repo digests from `metadata.txt`.

How `pinned` works (`jobs.Pipeline.preparePinning`; the stack's own
`compose.yml` is never modified). Steps 1–3 run after the restore into the
staging directory and **before the swap**, so any failure (no or invalid
`metadata.txt`, an image that cannot be pulled, `COMPOSE_FILE` in `.env`)
aborts the restore with the normal rollback: staging removed, the old stack
started again. In place there is nothing to go back to: the stack is left
stopped with the restored files (see "In-place restore").

1. Parse `metadata.txt` from the staging directory (`ParseMetadata`, every
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
  can retry with `tags`. A per-service "continue with tags" is a possible
  later refinement.
- Private registries: the worker needs registry credentials (mount a Docker
  `config.json` read-only into the worker if required). Document this.
- Compose `up -d` later, run manually with only `compose.yml`, will detect an
  image change and recreate containers with the tag version. To make pinning
  permanent, offer an explicit second action "write compose.override.yml with
  pinned images" (never automatic, since an override file may already exist).
- Multi-arch: the repo digest recorded from `docker image inspect` identifies
  the manifest that was pulled; pulling by it yields the same platform content.
- Also applies to cross-host migration: pinned is the recommended mode there.
- Without `metadata.txt` (very old snapshots) a pinned restore fails before the swap (the UI cannot know in advance without reading the snapshot).

## Update pipeline

Job type `update` (`jobs.Pipeline.Update`), started by users with the update
right (`can_update`) from the "Update" tab (see "Update check"). It runs in
a worker like backup and restore (same queue, per-stack lock, live log,
cancel):

1. With a repository: check it. Record the stack state; the stack must have
   running services (an update recreates the running services; a stopped
   stack is refused before anything happens). Write `metadata.txt`: its
   image digests come from the running containers, i.e. the versions
   **before** the update.
2. `docker compose pull --ignore-buildable --quiet -- <running services>`
   while the stack keeps running. Failure: the job fails, nothing changed.
   With the option **only if new** (`only_if_new`, worker flag
   `--only-if-new`, preselected in the UI) the worker then compares, per
   running service, the id of the local image its compose file names
   (`docker image inspect <ref>`, i.e. what `up` would use) with the
   container's image id, and the container's image reference with the
   compose file's. A service also counts as changed when its container
   runs another reference, e.g. a version pinned by digest by a restore or
   rollback: recreating it switches back to the tag ("unpin"). If no
   service changes, the job ends here as `succeeded` with
   `result.unchanged`: no snapshots, nothing stopped. Services without an
   image (built locally) do not count.
3. With a repository: pre-stop snapshot (failure only warns). It can be
   turned off (`no_pre_stop`, worker flag `--no-pre-stop`, checkbox in the
   update form and in update schedules); the final snapshot then has no
   parent and copies everything with the stack stopped.
4. `compose stop`.
5. With a repository: final snapshot (parent = pre-stop): the state the
   rollback returns to. Failure in 4–5 (or cancellation before 6): `compose
   start` of the old containers (still the old images), job fails.
6. `compose up --detach --force-recreate -- <running services>`, then wait
   until every service runs and, with a healthcheck, is healthy (5 min;
   a service that exits or turns unhealthy fails at once).
7. Failure in 6:
   - with repository: **automatic rollback** = the restore pipeline on the
     final snapshot without safety snapshot (the final snapshot already is
     the state before the update), with image pinning (old digests from its
     `metadata.txt`), carrying the manager's job files over if it is the
     manager's own stack. By default the snapshot is restored into a new
     directory and swapped in; the failed version's directory is kept as
     `<stack>.old-<time>`. With the option **roll back in place**
     (`rollback_in_place`, worker flag `--rollback-in-place`) it is
     restored in place (see "In-place restore"): only the few files
     changed since the final snapshot are rewritten, nothing of the failed
     version is kept, and a rollback that fails half-way leaves the stack
     stopped. The rollback also runs if the job was cancelled during step 6
     (own timeout, 2 h). The job ends `failed` with `result.rolled_back`.
   - without repository: **no rollback at all**, not even starting the old
     images (they could damage data a new version already migrated). The
     job fails and the stack stays as it is.

Without a repository the update is only possible after explicit
confirmation on the confirmation page ("no backup and no rollback";
`JobOptions.NoBackup`). The dispatcher decides when the job starts: a stack
that has a repository by then is always backed up; one without repository
and without confirmation fails. The worker gets `no_backup` in the spec
explicitly; without it, missing repository settings fail the job instead of
silently skipping the backup.

Not covered: images built locally (`build:`) are not rebuilt; a service
that starts, passes the check and fails later is not detected.

### Update check

The "Update" tab (update right) has a **Check now** button (`POST
/stacks/{name}/update/check`, CSRF token because every check sends
registry requests; not audited, it changes nothing). The manager
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
   as pinned. If running services are pinned, the tab offers **Unpin
   now** (with a repository): it queues an update with "only if new" and
   rollback by swap, which recreates them with their tag (and, as any
   update, pulls newer images of the tags). The table shows running and available version: the version
   label if the image has one, else creation date and short digest.

At most one check per user and three in total run at a time. The check is
a preview: the update job pulls and compares again itself ("only if new").
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
   With a repository, the checkbox "roll back in place" sets
   `rollback_in_place` (see "Update pipeline", step 7).
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
  services the new file adds; waited for (running/healthy, 5 min) are the
  previously running ones (new services may be one-shot tasks). Containers
  of removed services stay stopped (logged; remove them with `docker
  compose up --remove-orphans`);
- if they do not come up: rollback with a repository (restore of the final
  snapshot, which brings back the old compose file, with pinned images), no
  rollback without one.

## Snapshot cache

Listing snapshots on a remote repository can be slow, so the manager keeps a
cache in the `snapshots` table (id, short id, time, host, paths, tags, size
summary if available).

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
  too) and `hostname` = `STACKVAULT_HOSTNAME` (`store.StackSnapshots`). The
  page says how many other snapshots were hidden. The worker checks the
  path again before stopping anything.

## Restore from another host (stack migration)

**Not implemented; planned as the first feature after release 0.1.**

Use case: move a single stack from host A to host B (or rebuild a host).

- Host B is configured with the same repository (location + credentials).
- The stack name must stay the same. Import refuses a snapshot whose last
  path component (`paths[0]`) differs from the stack name being imported.
  Renaming on import is intentionally not supported.
- Admin action **Import stack**: create a stack record for a name that does not
  exist on disk yet (`present = 0`), attach the repository, refresh the snapshot
  cache. Snapshots from other hosts are listed (no `--host` filter); host and
  original path are shown per snapshot.
- Restore then runs the normal pipeline minus stop, safety snapshot and swap,
  because there is nothing to protect: restore into staging, rename to
  `<STACKS_ROOT>/<stack>`.
- By default the stack is **not started** automatically. Offer "Restore and
  start" as an explicit checkbox.
- After restore, compare `metadata.txt` (image digests of the source) with what
  is available locally and show a warning if images differ or are missing
  (`docker compose pull` is a separate, explicit action).
- Known caveats to surface in the UI: numeric UID/GID are restored as-is (users
  on host B may differ); host-specific settings in compose files (ports,
  external networks, absolute paths outside the stack directory) can break;
  only bind mounts inside the stack directory are covered.
- Importing/creating stacks is admin-only.

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

Light and dark mode follow the browser/OS setting (`prefers-color-scheme`,
no switch in the UI). All colors in `static/style.css` are CSS variables
with a light and a dark set; the job log is a dark panel in both modes.

| Route | Who | |
|---|---|---|
| `GET /` | logged in | stacks the user may view (admins: all) |
| `GET /stacks/{name}` | view right | "Backups" tab: repository location, cached snapshots (same for every user) |
| `POST /stacks/{name}/backup` | backup right | queue a backup (option: pre-stop snapshot) |
| `GET /stacks/{name}/logs` | logs right | "Logs" tab: container checkboxes and live log panel |
| `GET /stacks/{name}/logs/events` | logs right | live container logs (SSE), see "Container logs" |
| `GET /stacks/{name}/update` | update right | "Update" tab: update check and update form (only if new, rollback method); without repository it requires confirming "no backup, no rollback" |
| `POST /stacks/{name}/update/check` | update right | check for newer images (registry query, no pull), see "Update check" |
| `POST /stacks/{name}/update` | update right | queue an update, see "Update pipeline" |
| `GET /stacks/{name}/restore?snapshot=<id>` | restore right | confirmation page with options |
| `POST /stacks/{name}/restore` | restore right | queue a restore of a cached, not removed snapshot; only admins may skip the safety snapshot |
| `GET /jobs/{id}` | view right on the job's stack | job details and log; followed live by `static/joblog.js` while not final |
| `GET /jobs/{id}/events` | view right on the job's stack | live stream of the job (SSE), see "Live logs" |
| `POST /jobs/{id}/cancel` | backup/restore/update right (by job type; compose-apply: admin) | cancel a queued or running job |
| `POST /stacks/{name}/snapshots/refresh` | view right | refresh the snapshot cache (read-only restic; at most once per 10 s per stack; HTMX: returns the snapshot section) |
| `GET /stacks/{name}/shell` | admin, opt-in | "Shell" tab: containers, terminal of `?container=<id>` |
| `GET /stacks/{name}/shell/ws` | admin, opt-in | WebSocket of a shell session, see "Container shell" |
| `GET /stacks/{name}/compose` | admin | "Compose" tab: editor for the main compose file |
| `POST /stacks/{name}/compose` | admin | validate and queue a compose-apply job, see "Compose editor" |
| `GET /stacks/{name}/settings` | admin | "Settings" tab: repository, schedules with next run, enable/disable, per-user permissions |
| `POST /stacks/{name}/repository` | admin | create/update the repository, then refresh the snapshot cache as an access check |
| `POST /stacks/{name}/repository/init` | admin | `restic init` at the stored location (offered only when no repository exists there) |
| `POST /stacks/{name}/permissions` | admin | set one user's backup/restore right (HTMX: returns the table row) |
| `POST /stacks/{name}/enabled` | admin | enable/disable the stack for scheduled backups |
| `POST /stacks/{name}/schedules` | admin | add a schedule (cron expression, pre-stop option); at most 20 per stack |
| `POST /stacks/{name}/schedules/{id}/enabled` | admin | enable/disable one schedule |
| `POST /stacks/{name}/schedules/{id}/delete` | admin | delete one schedule |
| `GET/POST /admin/users` | admin | list users, create a user with a generated one-time password |
| `POST /admin/users/{id}` | admin | set admin/disabled flags (not on one's own account) |
| `POST /admin/users/{id}/password` | admin | generate a new password (must be changed, sessions end) |

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

Stack pages share a header with tabs (`stack-header` in `partials.html`).
The tab list is built per user in `newStackHeader`; a tab is only shown if
the user may open it, and its handler checks the permission again. Tabs:
"Backups" (view right), "Logs" (logs right), "Compose", "Shell" (only with
`STACKVAULT_ENABLE_SHELL`) and "Settings" (admin). A planned "Status" tab (restart) is added there with its own
permission.

Stacks a user may not view answer 404 (not 403), so stack names are not
revealed. `{name}` must pass `stacks.ValidName` and exist in the database
(i.e. was discovered).

Static assets are vendored into `internal/web/static` and embedded:
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

## Live logs

The worker appends to `/job/log.txt` as JSON lines (`log/slog`; secrets
redacted before writing).

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
  page). Log files are kept per job; cleanup by age is a later option.

## Container logs

The "Logs" tab (`can_logs`, admins always) shows the live output of all
containers of a stack, mixed in the order `docker compose logs` prints
them. One checkbox per container shows or hides its lines; filtering
happens in the browser only (`static/stacklog.js`), the stream always
carries all containers. "Wrap lines" (on by default, remembered per
browser in `localStorage`) can be turned off for horizontal scrolling.
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

## Container shell

Opt-in (`STACKVAULT_ENABLE_SHELL=true`, default off) and admin-only: the
"Shell" tab opens an interactive shell in a running container of the
stack. Without the setting the tab and its routes do not exist (an
exception to "disabled, not hidden": the feature is meant to be invisible
where it is not wanted).

- `GET /stacks/{name}/shell` lists the stack's containers
  (`docker.CLI.StackContainers`: `compose ps --all` after the project owner
  check). `?container=<id>` shows the terminal of a running container of
  that list (xterm.js, `static/shell.js`).
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

## Scheduler

`internal/scheduler`, using `robfig/cron/v3` in the manager. A schedule only
creates a `queued` job (trigger `schedule`, no requesting user); execution
always goes through the same queue, per-stack lock and worker path as manual
runs. A schedule has a job type (Settings tab, admins):

- **backup**, with the option "pre-stop snapshot";
- **update**, always with "only if new" (a scheduled update never stops or
  backs up a stack when no running service gets a new image or is pinned),
  with the options "pre-stop snapshot" (`no_pre_stop` in the job when off)
  and "roll back in place". It needs a repository: without one the job
  fails (nobody confirms an update without backup), and it fails if the
  stack is not running.

- Expressions: the standard 5 fields (minute hour day-of-month month
  day-of-week) or descriptors (`@daily`, `@weekly`, `@every 6h`, ...).
  `scheduler.Parse` validates them before saving: no seconds field, no
  per-schedule `CRON_TZ=`/`TZ=` prefix, `@every` at least 1 minute, at most
  100 characters, and the expression must match some date. Expressions are
  stored normalized (single spaces).
- Time zone: the manager's local zone (`TZ`). The UI shows times in UTC and
  names the manager's zone next to the schedule form.
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
in `.restore-tmp` or containing `.old-` are reserved for the restore
pipeline's staging and previous-version directories and are never
discovered, so a restore does not create new stacks. Stacks can be
disabled per stack. Discovery is the only source of stack names/paths.

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
elsewhere on the host or one started with `docker compose -p`. `restic restore <id>:<subpath>`
requires restic >= 0.17.

## Worker CLI (M1)

```
worker run <job-dir>
worker backup  [--pre-stop] <stack>
worker restore [--no-safety-snapshot] [--no-start] [--pin-images] [--in-place] <stack> <snapshot-id>
worker update  [--no-backup] <stack>
```

`run` is the manager's interface (see "Job handoff"); `backup`, `restore`
and `update` are for manual use and tests. The repository settings
(`RESTIC_REPOSITORY`, `RESTIC_PASSWORD(_FILE)`) are optional in the worker's
configuration, but every job needs them except `update --no-backup`.

Flags go before positional arguments. Exit codes: 0 success, 1 job failed,
2 usage/configuration error. Logs are JSON lines (`log/slog`) on stderr.
The repository always comes from the environment:

| Variable | Meaning |
|---|---|
| `STACKS_ROOT` | absolute stacks root (required) |
| `STACKVAULT_HOSTNAME` | value for `restic --host` (defaults to `os.Hostname()`, which is random inside a container, so set it there) |
| `RESTIC_REPOSITORY` | repository (required) |
| `RESTIC_PASSWORD_FILE` / `RESTIC_PASSWORD` | one is required |
| backend variables (`AWS_*`, `B2_*`, `AZURE_*`, `GOOGLE_*`, `RESTIC_REST_*`) | passed to restic; values are redacted from output |
| `STACKVAULT_JOB_TIMEOUT` | hard job timeout, Go duration, default `6h` |
| `STACKVAULT_LOG_LEVEL` | `debug`, `info`, `warn`, `error` |

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

Not yet implemented: restore into a not-yet-existing stack (import), see
"Restore from another host" and "Planned features".

## Planned features (not implemented)

Ideas agreed on for later releases. Design notes are a starting point, not
a commitment.

### Restore from a different host

Stack migration as described in "Restore from another host (stack
migration)" together with "Source host for migration" below: import a
stack that does not exist on this host yet, list another host's snapshots
of the same stack name, restore into a new stack directory. Deliberately
not part of release 0.1.

### Time placeholders in the repository location + auto-init

Goal: a new repository per period (e.g. per year) managed by the scheduled
backups alone, so repositories and their indexes stay small.

- Placeholders in the location, e.g.
  `rest:https://backup.example/web-{year}`. Candidates: `{year}`,
  `{month}` (two digits), maybe `{quarter}`. Only a fixed set of
  placeholders, no expressions. Validation must run on the expanded
  location too (no credentials, no leading `-`).
- Expansion time: when the job is created (manager), stored in the job
  spec, so a backup running over midnight on Dec 31 uses one location for
  all its snapshots. Time zone: the manager's configured zone (new setting
  or `TZ`), documented, since "year" changes at local midnight.
- Per-repository setting **auto-init if missing** (off by default): if the
  expanded location has no repository (restic exit code 10), the worker
  runs `restic init` with the stored password before the backup and logs
  it; audit entry `repository.init` with trigger `schedule`. Any other
  error (wrong password, network) never triggers init.
- Consequences to design:
  - Snapshot cache and restore must see **all** periods, not only the
    current one: keep a list of known expanded locations per repository
    (e.g. a `repository_locations` table filled when a location is first
    used or initialized), refresh and show snapshots across them, and
    restore from the location the snapshot belongs to.
  - The first backup in a new period has no parent snapshot (full upload);
    the pre-stop/final parent chain only applies within one location.
  - Same password for all periods (one stored secret); changing it applies
    to new periods only, old repositories keep their old password. Either
    keep old secrets per known location or document the limitation.
  - The manual "Initialize repository" button (current behaviour) stays
    for locations without placeholders.

### Source host for migration

Snapshots are filtered by this host's `STACKVAULT_HOSTNAME`. To move a stack
from host A to host B, B must list A's snapshots of the same stack name: a
per-repository (or per-import) setting **source host** (default: this host)
selects whose snapshots are listed and restored. Restoring another host's
snapshot is an admin action; new backups always use this host's name.

### Rollbacks: in place or with swap (open decision)

Since 2026-11-16 the user chooses per job: the update form and the compose
editor have the option "roll back in place" (default: swap). Still open:

- a per-stack default (a stack setting "large data" that preselects in
  place on the update, compose and restore forms), useful once scheduled
  updates exist;
- what to keep for inspection when an in-place rollback replaces the
  failed version (nothing today; with a swap it is `<stack>.old-<ts>`).

### Progress and feedback for long operations

Requested 2026-11-16: the UI should show progress (percent, speed,
remaining time) or at least messages for operations that take long, and a
spinner where no progress is known. Today a job shows its log lines as they
come, but nothing between them, and synchronous actions show nothing until
the page returns.

With real progress (the tools report it):

- **Backup** (pre-stop and final snapshot): `restic backup --json` already
  prints `message_type: "status"` lines (`percent_done`, `bytes_done`,
  `total_bytes`, `files_done`, `seconds_remaining`); `restic.CLI` drops
  them. Idea: the worker writes the latest status (throttled, e.g. every
  2 s) to a small `progress.json` in the job directory, the job page's
  live stream sends it as its own SSE event, and the page shows a progress
  bar with speed and remaining time.
- **Restore**, also in place and as a rollback: `restic restore --json`
  prints status lines too (restic >= 0.17: `percent_done`, `bytes_restored`,
  `total_bytes`); same mechanism.
- **Pulling images** (update, pinned restore): runs with `--quiet` today.
  `docker compose --progress json pull` (and `docker pull` without
  `--quiet`) report per-layer progress; summarize it as "image x: n of m
  MB".

Synchronous requests that only need a spinner or a message (the page waits
for the answer):

- **Check for updates** (about 4 s per stack, longer with many images): a
  spinner and a disabled button while it runs; better, per-service rows
  that fill in as each registry answers (SSE or HTMX polling).
- **Saving a repository** (access check by listing snapshots), **Initialize
  repository** (several seconds) and **Refresh snapshots**: slow with
  remote repositories.
- **Validate and apply** in the compose editor (`docker compose config`).
- **Opening a shell** ("connecting…" exists) and the **log view** before the
  first lines arrive.

Phases of a job that wait without output and could say what they wait for:

- **Queued** behind another job of the stack, or behind the global limit
  (`STACKVAULT_MAX_JOBS`): show the position or the blocking job.
- **Stopping the stack** (`compose stop`, up to the stop timeout of each
  container) and **waiting until healthy** (up to 5 minutes for updates):
  show the services still pending.
- **Rollback** after a failed update: its own phase marker on the job page.

### Sizes and durations in the snapshot and job lists

Requested 2026-11-16. Most of the data already exists:

- **Snapshot list**: add **size** and **duration** per snapshot. Snapshots
  made with restic >= 0.17 carry a `summary` in `restic snapshots --json`
  (checked with 0.18): `data_added_packed` is what the snapshot added to
  the repository after deduplication and compression, i.e. what was
  transferred and stored (not the size on the source disk);
  `backup_end - backup_start` is the duration. The snapshot cache
  (`snapshot_cache`) needs columns for them, filled on refresh; older
  snapshots without `summary` show "–". Note: a final snapshot after a
  pre-stop snapshot adds little (only what changed in between), so the
  list should make clear that the size is "added", not the full size.
- **Job list on the Backups tab**: a **duration** column
  (`finished_at - started_at`, "running for …" while it runs). No schema
  change.
- **Size of the stack directory**: `total_bytes_processed` of the newest
  snapshot is the size of the stack directory at backup time (files
  processed, before deduplication). Show it on the Backups tab ("Stack
  size at the last backup: 315 MB"), maybe also in the stack list. The
  manager does not need to walk the directory itself.

### Compose editor: skip repeated backups

The log view, the update and the compose editor (agreed 2026-11-16) are
implemented, see "Container logs", "Update pipeline" and "Compose editor".
Open idea for the editor: an option to skip the backup when the same stack
was edited and backed up a short time ago, for quick fix-up edits (the
rollback would then return to that earlier backup).

A "Status" tab (running services, restart) remains open.
