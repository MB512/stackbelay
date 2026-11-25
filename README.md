# StackBelay

> Never climb without a belay. Never update without a backup.

Self-hosted web app that backs up Docker Compose stacks to
[restic](https://restic.net) repositories, and restores them.

A *stack* is a directory with a compose file (`compose.yaml`,
`compose.yml`, `docker-compose.yml` or `docker-compose.yaml`) under one
stacks root (e.g. `/home/docker/<stack>/compose.yml`). StackBelay backs up the whole
directory, including bind-mounted data inside it, with the stack stopped,
so the backup is consistent without database-specific dumps.

Status: **0.1**. Use it next to, not instead of, a backup strategy you
already trust until it has proven itself on your hosts.

## Features

- **Backups per stack** into restic repositories that admins manage in
  one place: several stacks (and hosts) may share a repository, and a
  change of it (e.g. a new location per year) applies to all of them.
  Pipeline: `metadata.txt` (image digests, service states) -> snapshot
  while running (shorter downtime) -> `compose stop` -> final
  snapshot -> `compose start`. The stack is started again in every case,
  also when the backup fails or is cancelled.
- **Restores** in place, after a safety snapshot of the current state: only
  files that differ from the snapshot are downloaded and rewritten, so a
  restore of a large stack is fast, and one that was interrupted continues
  where it stopped when it is run again. Optionally with the **image
  versions from backup time** (pinned by digest).
- **Move a stack to another host** (or rebuild a host) through its
  repository: import it, sync it with repeated restores while it
  still runs on the old host, take over after the final backup.
- **Updates** with backup and automatic rollback: pull newer images, back
  up, recreate the services, roll back to the backup if they do not come
  up. An update check shows which services have newer images (without
  pulling); an update skips everything when no image changed. A stack
  with a repository is only updated with a backup: while its repository
  is unreachable, its updates fail without changing anything (#43).
- **Schedules** (cron expressions) per stack for backups and updates, plus
  manual runs. Jobs are queued; one job per stack at a time.
- **Stack list** with the state of every stack (started, stopped,
  degraded, …), its schedules, last backup and last update.
- **Status page** per stack: containers with state, health and update
  check, recent jobs; **up, start, restart, stop and down** (without
  backup).
- **Sizes and durations**: what each snapshot added and how long it took,
  the stack size at the last backup and on disk, job durations.
- **Live container logs** per stack, with a filter per container, and a
  **global log view** of several stacks.
- **Host cleanup**: removes stopped orphan containers and old unused
  images, with a preview; never volumes.
- **Notifications** through [ntfy](https://ntfy.sh) for failed jobs,
  rollbacks and stacks a job left down (successes optional), grouped when
  several jobs finish together; per-stack mute, and a temporary mute for
  admins while they work. A daily or weekly
  **summary** of what needs attention, backups, downtime, updates,
  changes, security events and the data added per repository. A
  **stack monitor** reports stacks that degrade, stop or vanish outside
  StackBelay's jobs, and when they run again.
- **Compose editor** and an opt-in **container shell** for admins.
- **Multi-user** with per-stack rights (backup, restore, update, operate,
  logs);
  admins manage users, repositories, schedules and rights.
- **Live job logs** in the browser, job history; admin pages with the job
  queue of all stacks and the audit log.
- Backup and restore run in a separate, short-lived **worker container**.
  StackBelay can therefore back up and restore **its own stack**.
- Only creating and reading restic operations: works with
  [rest-server](https://github.com/restic/rest-server) in **append-only**
  mode. Retention (`forget`/`prune`) is left to the repository owner, e.g.
  with its companion [BelayPoint](https://github.com/MB512/belaypoint), which
  manages the rest-server and runs `forget`/`prune`/`check` there.

Not supported: retention, Windows, Kubernetes. Planned features, bugs and
design discussions are tracked in
[GitHub issues](https://github.com/MB512/stackbelay/issues).

## Requirements

- Linux host with Docker Engine and the Compose plugin. Docker Desktop is
  not supported for jobs (see `docs/architecture.md`).
- All stacks below one directory (`STACKS_ROOT`), each with a compose
  file named `compose.yaml`, `compose.yml`, `docker-compose.yml` or
  `docker-compose.yaml` (the names `docker compose` looks for); only data inside the stack directory is
  backed up (bind mounts outside it and named volumes are not).
- One or more restic repositories (shared by stacks or one per stack), e.g. on a rest-server
  started with `--append-only`.
- A TLS reverse proxy in front of the web UI.

## Quick start

1. Create the stack directory:

   ```sh
   mkdir -p /home/docker/stackbelay && cd /home/docker/stackbelay
   ```

2. Copy [`deploy/compose.example.yml`](deploy/compose.example.yml) to
   `/home/docker/stackbelay/compose.yml` and adjust `STACKS_ROOT`, the
   matching volume, `STACKBELAY_HOSTNAME` and `TZ`.

3. Start it and read the initial admin password from the log:

   ```sh
   docker compose up -d
   docker compose logs stackbelay | grep "INITIAL ADMIN PASSWORD"
   ```

   On the first start StackBelay also generates the secret key that
   encrypts the stored repository passwords: `data/secret.key` (mode 0600).
   **Keep a copy somewhere safe**, outside this host. Without it the stored
   repository passwords cannot be decrypted, and StackBelay refuses to start
   with a database whose key is missing or wrong instead of creating a new
   one.

4. Open the UI through your reverse proxy, log in as `admin`, set a new
   password. Add a repository under **Admin** -> **Repositories** (name,
   location and password; "Initialize repository" creates a new one). For
   each stack: **Settings** -> choose the repository, add schedules and
   user rights. Then **Back up now** on the stack's Status tab.

### Optional: keep the key out of the backups

`data/secret.key` lies inside the StackBelay stack, so it is part of that
stack's own backups (protected by its restic repository password). That
makes restoring StackBelay itself simple. To keep the key in no backup at
all, store it outside `STACKS_ROOT` and point `STACKBELAY_SECRET_KEY_FILE`
at it:

```sh
sudo install -d -m 700 /etc/stackbelay
openssl rand -base64 32 | sudo tee /etc/stackbelay/secret.key >/dev/null
sudo chmod 600 /etc/stackbelay/secret.key
```

On an existing installation, move the generated key there instead
(`sudo mv data/secret.key /etc/stackbelay/secret.key`) — a new key would not
match the database. Then enable the commented lines in the compose file.

The image is `ghcr.io/mb512/stackbelay` (linux/amd64, linux/arm64). It
contains the manager (web UI, scheduler) and the worker; the manager starts
its workers from its own image.

### Private registries

To pull images from a private registry (updates, pinned restores, the
update check), give the manager the docker client configuration with the
registry credentials. Mount either a directory or a single file (read-only):

```yaml
    volumes:
      - /root/.docker/config.json:/root/.docker/config.json:ro
```

The manager passes the same mount, read-only, to every worker container.
The file must contain the credentials itself (`"auths"`, as written by
`docker login` without a credential helper): credential helpers such as
`credsStore: desktop` or `pass` do not exist inside the containers. Create
it e.g. with `DOCKER_CONFIG=/etc/stackbelay/docker docker login
registry.example.com` and mount `/etc/stackbelay/docker` at
`/root/.docker`.

Tags: `<major>.<minor>.<patch>`, `<major>.<minor>` and `latest` for
releases. `dev` is an untested development build of an arbitrary commit,
moved to each new dev build without a version number (the UI shows
`dev-<commit>`); do not use it in production.

## Configuration

Environment variables only.

| Variable | Meaning |
|---|---|
| `STACKS_ROOT` | absolute stacks root, mounted at the **same path** in the container (required) |
| `STACKBELAY_HOSTNAME` | name of this Docker host, recorded in every snapshot and named in notifications (required) |
| `STACKBELAY_SECRET_KEY_FILE` / `STACKBELAY_SECRET_KEY` | 32-byte key, base64; at most one. Default: `<STACKBELAY_DATA_DIR>/secret.key`, generated on the first start |
| `STACKBELAY_DATA_DIR` | database and job files, default `/data` |
| `STACKBELAY_LISTEN` | listen address, default `:8080` |
| `STACKBELAY_SESSION_TTL` | session lifetime, default `12h` |
| `STACKBELAY_ALLOW_INSECURE_HTTP` | accept logins over plain HTTP (testing only), default `false` |
| `STACKBELAY_WORKER_IMAGE` | image for worker containers, default: the manager's own image |
| `STACKBELAY_JOB_TIMEOUT` | hard limit for one job (backup, restore, update, ...), e.g. `72h`, default `48h`; a job running longer is stopped (the stack is restarted as after any failure) |
| `STACKBELAY_MAX_JOBS` | jobs running at the same time across all stacks (disk and network load), default `3` |
| `STACKBELAY_MAX_DOWNTIMES` | jobs that may keep a stack stopped at the same time, default `1`: a backup or update takes its snapshot while the stack runs and only then waits for this slot, so a long backup or restore does not hold up the other stacks |
| `STACKBELAY_DOWNTIME_TIMEOUT` | limit for the time a backup or update keeps a stack stopped until its final snapshot, e.g. `2h`, default `1h`; when it runs out, the final snapshot is aborted and the stack started again (the job fails) |
| `STACKBELAY_JOB_LOG_RETENTION` | logs of jobs that finished longer ago are deleted (the job list keeps them, without log), e.g. `720h` for 30 days, default `2160h` (90 days), `0` keeps them forever |
| `STACKBELAY_CLEANUP_IMAGE_MIN_AGE` | host cleanup: keep unused untagged images pulled or replaced by an update less than this long ago, whole hours, default `168h` |
| `STACKBELAY_ENABLE_SHELL` | `true` gives admins a shell in stack containers ("Shell" per container on the Status tab), default `false` |
| `STACKBELAY_LOG_LEVEL` | `debug`, `info`, `warn`, `error` |
| `TZ` | time zone of the schedules and of all times shown in the UI, e.g. `Europe/Berlin` (default UTC); an unknown zone stops the start |

## Security

The manager needs the Docker socket, which is equivalent to root on the
host. StackBelay is designed so that web users cannot turn that into
arbitrary commands: only predefined jobs on discovered stacks, workers from
a fixed template, no shell, CSRF protection, per-stack rights and an audit
log. Repository credentials are encrypted at rest and never passed on
command lines or written to logs. Details: [`docs/security.md`](docs/security.md).
Please report vulnerabilities privately, see [SECURITY.md](SECURITY.md).

## Documentation

- [`docs/architecture.md`](docs/architecture.md): components, job pipeline,
  restore, image pinning, scheduler, web routes
- [`docs/data-model.md`](docs/data-model.md): database schema
- [`docs/security.md`](docs/security.md): threat model and rules

## Development

Go 1.26+, Docker with Compose, restic.

```sh
go build ./...
go vet ./...
go test ./... -race
golangci-lint run
docker build -f deploy/Dockerfile -t stackbelay:dev .
scripts/e2e-local.sh      # worker pipeline against the local Docker daemon
scripts/e2e-dispatch.sh   # manager + worker containers through the web UI
scripts/e2e-self.sh       # StackBelay backs up and restores itself
scripts/e2e-update.sh     # update and compose-apply jobs: success, rollback, no backup
```

Development guidelines: [`CLAUDE.md`](CLAUDE.md).

## License

[MIT](LICENSE)
