# StackVault (working title)

Self-hosted web app (Go) that backs up Docker Compose stacks from a host to
[restic](https://restic.net) repositories. Multi-user, per-stack permissions,
per-stack repositories, cron schedules plus manual runs. Open source (GitHub).

Read `docs/architecture.md`, `docs/data-model.md` and `docs/security.md`
before changing anything structural. Keep them in sync with the code.

## Maintainer context

The maintainer is an experienced C#/.NET developer (also C, C++, Java) but new
to Go and to web development. Therefore:

- Prefer boring, idiomatic standard-library solutions over clever ones.
- When introducing a Go idiom the first time (error wrapping, `defer`,
  goroutines/channels, `context`, small interfaces), add a one-line
  explanation in your summary, ideally with the C#/.NET equivalent. Use C#
  only for comparisons and examples, not Rust or other languages.
- Frontend: server-rendered `html/template` + HTMX. No SPA, no Node build step,
  no CDN links (vendor static assets into `internal/web/static`).

## Core concept

Two roles, one image, two binaries:

- `manager`: web UI, auth, scheduler, job creation. Never runs backups itself.
- `worker`: one-shot process in a **separate container** (started by the
  manager via the Docker socket). Performs backup/restore. Because it is
  independent of the manager, it can stop, back up, restore and restart the
  manager's own stack.

Backup pipeline (per stack): metadata.txt -> optional pre-stop snapshot ->
`compose stop` -> final snapshot (parent = pre-stop snapshot) -> `compose start`.
Restart must ALWAYS happen (defer), even on failure. See `docs/architecture.md`.

## Tech decisions (do not change without discussion)

- Go 1.26+ (required by `modernc.org/sqlite`), module `github.com/MB512/stackvault`
- Router: `go-chi/chi`; templates: `html/template`; UI: HTMX
- DB: SQLite via `modernc.org/sqlite` (pure Go, no CGO), embedded SQL migrations
- Scheduler: `robfig/cron/v3`
- Passwords: argon2id (`golang.org/x/crypto`); secrets at rest: AES-256-GCM
- Docker and restic are driven through their **CLIs** (`docker compose ...`,
  `restic ... --json`), wrapped behind small interfaces (`docker.Client`,
  `restic.Runner`) so the job logic is unit-testable with fakes.
- Logging: `log/slog`. Config: environment variables only (12-factor).

## Layout

```
cmd/manager/        entrypoint: web server + scheduler
cmd/worker/         entrypoint: executes one job spec, then exits
internal/config/    env parsing
internal/store/     SQLite access, migrations (embed)
internal/auth/      sessions, argon2id, RBAC checks
internal/secrets/   AES-GCM encrypt/decrypt for repo credentials
internal/stacks/    discovery of stacks under STACKS_ROOT
internal/docker/    Client interface + CLI implementation
internal/restic/    Runner interface + CLI implementation, JSON parsing
internal/jobs/      job spec, state machine, backup/restore pipelines
internal/dispatch/  job queue runner: worker container template, start/watch/finish
internal/snapshots/ snapshot cache refresh (read-only restic, run by manager)
internal/scheduler/ cron -> job creation
internal/web/       handlers, templates, static assets
docs/               architecture, data model, security
deploy/             Dockerfile, compose.example.yml
```

## Commands

```
go build ./...
go vet ./...
go test ./... -race
golangci-lint run
docker build -f deploy/Dockerfile -t stackvault:dev .
```

Run `go vet` and `go test ./... -race` before every commit.

## Conventions

- `gofmt`/`goimports` clean. Wrap errors: `fmt.Errorf("doing X: %w", err)`.
- Pass `context.Context` as first parameter to anything that blocks or does I/O.
- Define interfaces where they are consumed, keep them small.
- No global mutable state, no `init()` side effects.
- Table-driven tests; pipeline tests use fake `docker.Client`/`restic.Runner`
  and must cover: failure after stop (stack still restarts), failure of
  pre-stop snapshot, restore rollback.
- Conventional Commits (`feat:`, `fix:`, `docs:`, `test:`, `chore:`).
- Small PRs/commits, one concern each.

## Hard rules (security-critical)

- Never build shell commands from user input. Use `exec.CommandContext` with an
  argument list, never `sh -c`. The only interactive execution is the
  opt-in, admin-only container shell (`STACKVAULT_ENABLE_SHELL`, see
  `docs/security.md` rule 13); never widen it without discussion.
- Stack names come only from discovery (directory scan), never free-form paths.
  Reject anything not in the discovered set; guard against path traversal.
- Repository passwords/credentials: encrypted in DB, passed to restic only via
  env or a tmpfs file in the worker, never as CLI args, never logged.
- Every state-changing endpoint: auth + per-stack permission check + CSRF
  token + audit log entry.
- Restore uses `restic restore <snap>:<snapshot path>`; never restore the
  absolute path tree and never trust a snapshot path without validating it
  against the snapshot's own `paths` from restic.
- `metadata.txt` is parsed by the restore. Keep its format versioned and
  stable (see `docs/architecture.md`); validate every field, treat it as
  untrusted input (it lives inside a snapshot). Image references used for
  pinning must match the digest syntax `^[a-z0-9./_-]+@sha256:[0-9a-f]{64}$`.
- The web process gets no direct Docker access beyond creating worker
  containers from a fixed template and read-only compose commands for
  discovered stacks (`ps`, `logs`, `config --quiet` for validation), plus
  `docker exec -it` for the opt-in admin shell, always with fixed argument
  lists. Anything that changes a stack (stop, up, pull,
  writing compose files) runs in a worker job. See `docs/security.md`.

## Milestones

1. **M1 Worker CLI**: `worker backup <stack>` / `worker restore <stack> <snap>`
   with fakes + tests; real Docker/restic implementation.
2. **M2 Store + stacks**: SQLite, migrations, stack discovery, repositories,
   secrets encryption, snapshot cache (refresh via read-only restic).
3. **M3 Manager**: auth, RBAC, spawn worker container, job status, live logs
   (SSE, tail of job log files).
4. **M4 Scheduler**: cron per stack, queue/locking per stack.
5. **M5 UI polish**: image pinning on restore via `metadata.txt` digests
   (done), unavailable actions shown disabled (done). Dashboards and a
   snapshot browser are not part of 0.1.
6. **M6 Release 0.1**: Dockerfile, GHCR publish via GitHub Actions, README, LICENSE.

After 0.1, the log view, the update job and the compose editor were added
(see `docs/architecture.md`). Next planned: **restore from a different host**
(stack migration: import of stacks from other hosts, restore into a
not-yet-existing stack, stack name must stay identical, source host
selection). See "Planned features" in `docs/architecture.md`.

## Out of scope (for now)

Windows hosts, Kubernetes, non-restic backends, database-native dumps
(file-level backup with the stack stopped is the consistency model), and
retention (`forget`/`prune`).

Retention is deliberately NOT handled by the app: repositories are expected to
be served by `restic/rest-server` in append-only mode, where clients cannot
delete or rewrite data. Do not add code that calls `restic forget`, `prune`,
`tag` or anything else that rewrites/deletes snapshots or pack files. Only
create and read operations (`init`, `backup`, `restore`, `snapshots`, `ls`,
`cat`, `check` read-only). `init` only runs as an explicit admin action when
restic reports that no repository exists at the location (restic refuses to
init over an existing one); it lets admins start e.g. a new repository per
year to keep repositories small. Pruning is done by the repository owner on
the server side.
