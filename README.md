# StackVault

Self-hosted web app that backs up Docker Compose stacks to
[restic](https://restic.net) repositories, and restores them.

A *stack* is a directory with a compose file (`compose.yaml`,
`compose.yml`, `docker-compose.yml` or `docker-compose.yaml`) under one
stacks root (e.g. `/home/docker/<stack>/compose.yml`). StackVault backs up the whole
directory, including bind-mounted data inside it, with the stack stopped,
so the backup is consistent without database-specific dumps.

Status: **0.1**, first release. Use it next to, not instead of, a backup
strategy you already trust until it has proven itself on your hosts.

## Features

- **Backups per stack** into one restic repository per stack (several
  stacks and hosts may share a repository).
  Pipeline: `metadata.txt` (image digests, service states) -> optional
  snapshot while running (shorter downtime) -> `compose stop` -> final
  snapshot -> `compose start`. The stack is started again in every case,
  also when the backup fails or is cancelled.
- **Restores** into a staging directory first, then swap: the previous
  directory is kept (`<stack>.old-<time>`), a safety snapshot of the current
  state is taken before. For large stacks a restore can run **in place**
  instead: only files that differ from the snapshot are downloaded and
  rewritten. Optionally with the **image versions from backup time**
  (pinned by digest).
- **Schedules** (cron expressions) per stack, plus manual backups. Jobs are
  queued; one job per stack at a time.
- **Multi-user** with per-stack rights (view, backup, restore); admins
  manage users, repositories, schedules and rights.
- **Live job logs** in the browser, job history, audit log.
- Backup and restore run in a separate, short-lived **worker container**.
  StackVault can therefore back up and restore **its own stack**.
- Only creating and reading restic operations: works with
  [rest-server](https://github.com/restic/rest-server) in **append-only**
  mode. Retention (`forget`/`prune`) is left to the repository owner.

Not in 0.1: restoring a stack from another host (migration), retention,
Windows, Kubernetes. Restore from another host is the first planned feature
after 0.1.

## Requirements

- Linux host with Docker Engine and the Compose plugin. Docker Desktop is
  not supported for jobs (see `docs/architecture.md`).
- All stacks below one directory (`STACKS_ROOT`), each with a compose
  file named `compose.yaml`, `compose.yml`, `docker-compose.yml` or
  `docker-compose.yaml` (the names `docker compose` looks for); only data inside the stack directory is
  backed up (bind mounts outside it and named volumes are not).
- A restic repository per stack (or shared), e.g. on a rest-server
  started with `--append-only`.
- A TLS reverse proxy in front of the web UI.

## Quick start

1. Create the stack directory:

   ```sh
   mkdir -p /home/docker/stackvault && cd /home/docker/stackvault
   ```

2. Copy [`deploy/compose.example.yml`](deploy/compose.example.yml) to
   `/home/docker/stackvault/compose.yml` and adjust `STACKS_ROOT`, the
   matching volume, `STACKVAULT_HOSTNAME` and `TZ`.

3. Start it and read the initial admin password from the log:

   ```sh
   docker compose up -d
   docker compose logs stackvault | grep "INITIAL ADMIN PASSWORD"
   ```

   On the first start StackVault also generates the secret key that
   encrypts the stored repository passwords: `data/secret.key` (mode 0600).
   **Keep a copy somewhere safe**, outside this host. Without it the stored
   repository passwords cannot be decrypted, and StackVault refuses to start
   with a database whose key is missing or wrong instead of creating a new
   one.

4. Open the UI through your reverse proxy, log in as `admin`, set a new
   password. For each stack: **Settings** -> repository (location and
   password; "Initialize repository" creates a new one), schedules and user
   rights. Then **Back up now** on the stack's Backups tab.

### Optional: keep the key out of the backups

`data/secret.key` lies inside the StackVault stack, so it is part of that
stack's own backups (protected by its restic repository password). That
makes restoring StackVault itself simple. To keep the key in no backup at
all, store it outside `STACKS_ROOT` and point `STACKVAULT_SECRET_KEY_FILE`
at it:

```sh
sudo install -d -m 700 /etc/stackvault
openssl rand -base64 32 | sudo tee /etc/stackvault/secret.key >/dev/null
sudo chmod 600 /etc/stackvault/secret.key
```

On an existing installation, move the generated key there instead
(`sudo mv data/secret.key /etc/stackvault/secret.key`) — a new key would not
match the database. Then enable the commented lines in the compose file.

The image is `ghcr.io/mb512/stackvault` (linux/amd64, linux/arm64). It
contains the manager (web UI, scheduler) and the worker; the manager starts
its workers from its own image.

## Configuration

Environment variables only:

| Variable | Meaning |
|---|---|
| `STACKS_ROOT` | absolute stacks root, mounted at the **same path** in the container (required) |
| `STACKVAULT_HOSTNAME` | name of this Docker host, recorded in every snapshot (required) |
| `STACKVAULT_SECRET_KEY_FILE` / `STACKVAULT_SECRET_KEY` | 32-byte key, base64; at most one. Default: `<STACKVAULT_DATA_DIR>/secret.key`, generated on the first start |
| `STACKVAULT_DATA_DIR` | database and job files, default `/data` |
| `STACKVAULT_LISTEN` | listen address, default `:8080` |
| `STACKVAULT_SESSION_TTL` | session lifetime, default `12h` |
| `STACKVAULT_ALLOW_INSECURE_HTTP` | accept logins over plain HTTP (testing only), default `false` |
| `STACKVAULT_WORKER_IMAGE` | image for worker containers, default: the manager's own image |
| `STACKVAULT_MAX_JOBS` | jobs running at the same time across all stacks, default `1` |
| `STACKVAULT_ENABLE_SHELL` | `true` gives admins a shell in stack containers ("Shell" tab), default `false` |
| `STACKVAULT_LOG_LEVEL` | `debug`, `info`, `warn`, `error` |
| `TZ` | time zone of the schedules, e.g. `Europe/Berlin` (default UTC) |

## Security

The manager needs the Docker socket, which is equivalent to root on the
host. StackVault is designed so that web users cannot turn that into
arbitrary commands: only predefined jobs on discovered stacks, workers from
a fixed template, no shell, CSRF protection, per-stack rights and an audit
log. Repository credentials are encrypted at rest and never passed on
command lines or written to logs. Details: [`docs/security.md`](docs/security.md).
Please report vulnerabilities privately, see [SECURITY.md](SECURITY.md).

## Documentation

- [`docs/architecture.md`](docs/architecture.md): components, job pipeline,
  restore, image pinning, scheduler, web routes, planned features
- [`docs/data-model.md`](docs/data-model.md): database schema
- [`docs/security.md`](docs/security.md): threat model and rules

## Development

Go 1.26+, Docker with Compose, restic.

```sh
go build ./...
go vet ./...
go test ./... -race
golangci-lint run
docker build -f deploy/Dockerfile -t stackvault:dev .
scripts/e2e-local.sh      # worker pipeline against the local Docker daemon
scripts/e2e-dispatch.sh   # manager + worker containers through the web UI
scripts/e2e-self.sh       # StackVault backs up and restores itself
scripts/e2e-update.sh     # update and compose-apply jobs: success, rollback, no backup
```

Development guidelines: [`CLAUDE.md`](CLAUDE.md).

## License

[MIT](LICENSE)
