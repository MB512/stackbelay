# Security

## Threat model (short)

Access to `/var/run/docker.sock` is equivalent to root on the host. The goal is
not to make that safe, but to ensure a compromised or malicious *web user*
cannot turn the app into a root shell.

Administrators are trusted with root-equivalent power: they can edit compose
files and, if the operator enables it (`STACKVAULT_ENABLE_SHELL`), open
shells in containers, including StackVault's own container with the Docker
socket. Protecting admin accounts (strong passwords, few admins, TLS) is
therefore part of protecting the host.

Assets: host root, stack data, repository credentials, backups themselves.
Attackers considered: authenticated low-privilege users, unauthenticated network
attackers reaching the UI, a leaked database file.

## Rules

1. **No free-form execution.** Users can only trigger predefined job types
   (`backup`, `restore`, `update`, `compose-apply`, `stop`, `start`,
   `restart`) on discovered stacks.
   The one exception is the container shell (rule 13). All external
   commands use `exec.CommandContext` with argument lists, never a shell.
2. **Input validation.** Stack names must match the discovered set. Snapshot IDs
   must match `^[0-9a-f]{8,64}$` and are verified against `restic snapshots`.
   Cron expressions are parsed and validated before saving (see
   "Scheduler" in architecture.md); the parser runs with a recover guard.
3. **Docker access.** Only the manager spawns workers, from a fixed template
   (image, mounts, labels, command). Users cannot influence mounts, image,
   entrypoint or environment (see "Dispatcher" in architecture.md). Job
   requests carry only the stack (from the database), the job type and
   validated options; the snapshot id must be in the stack's snapshot cache.
   Only admins may skip the pre-restore safety snapshot. Besides creating
   workers, the manager may run only read-only compose commands for
   discovered stacks, with fixed argument lists: `ps` for the Status tab,
   `config --format json` and `ps` for the State column of the stack list
   (cached 5 s per stack, at most 4 at a time, always after the project
   owner check), `logs` and `ps` for the log views (after checking that the compose project belongs to the stack
   directory; one stream per stack, the global log view merges up to 4; at
   most 4 streams per user, 20 in total) and `config --quiet`
   on a temporary copy outside the stack to validate an edited compose
   file. For the update check it also runs `docker image inspect` and
   `docker buildx imagetools inspect -- <ref>` (registry query, no pull) on
   the image references of a discovered stack's compose file; references
   that could be read as a flag or contain control characters are
   rejected. For the preview of the host cleanup (admin "Host" tab, issue
   #19) it lists read-only, with fixed argument lists: `docker image ls
   --filter dangling=true`, `docker ps --all --quiet`, `docker container
   inspect --format {{.Image}}` and `docker image inspect` on the ids those
   returned (validated), and per discovered stack, after the project owner
   check, `docker ps --all --filter label=com.docker.compose.project=<name>`
   (`web.Server.Cleanup` is a `jobs.CleanupLister`, which has no removal
   methods). Removing runs in the cleanup job in a worker: one `docker
   image rm -- <id>` or `docker container rm -- <id>` per validated id,
   without `--force`; never `prune`, never volumes. Container logs are not
   redacted, so viewing them is a separate per-stack right (`can_logs`).
   Everything that changes a stack runs in a worker job, also stopping,
   starting and restarting it (own right `can_operate`; these jobs get no
   repository credentials). The manager's own stack cannot be stopped from
   the UI (nobody could start it again there); restarting it is allowed.
   Editing a compose file is admin-only (it is equivalent to root on the
   host); the manager only validates it, the file is written by the
   compose-apply job in a worker, after a backup. The new content is
   checked against its SHA-256 in the worker, and an apply is refused if
   the file changed since it was opened in the editor.
4. **Secrets at rest.** Repo passwords and backend credentials are encrypted
   with AES-256-GCM using the secret key (`STACKVAULT_SECRET_KEY`,
   `STACKVAULT_SECRET_KEY_FILE`, or by default `<data>/secret.key`, mode
   0600, generated only on a fresh install; see "Secret key" in
   architecture.md). The key is not stored in the DB; a `key_check` row in
   `meta` lets the manager refuse to start with a key that does not match,
   and it never creates a new key next to an existing database. The key is
   never logged and never passed to workers.
   Each ciphertext is bound to its row via associated data, so an attacker
   with write access to the DB cannot move a secret to another stack's
   repository. Repository locations are plain text and must not embed
   passwords. restic gets only an allow-listed environment, so the secrets
   key never reaches a child process.
5. **Secrets in transit to worker.** Delivered via a tmpfs-mounted file or env
   of the worker container, removed after the job. Never CLI arguments (visible
   in `ps`), never logged. Log output from restic and the job logs are
   filtered for the secret values (`restic.SecretValues`: repository
   password, `RESTIC_REST_PASSWORD`, `AWS_SECRET_ACCESS_KEY`,
   `AWS_SESSION_TOKEN`, `B2_ACCOUNT_KEY`, `AZURE_ACCOUNT_KEY`,
   `AZURE_ACCOUNT_SAS`, `GOOGLE_ACCESS_TOKEN`). User names, key ids, regions
   and paths are not secret and not redacted (redacting a user name "demo"
   would also hide the stack name "demo-web").
6. **Authentication.** argon2id password hashes (m=64 MiB, t=3, p=2, PHC
   format); random 256-bit session tokens, stored as SHA-256 only; cookie
   `__Host-stackvault_session` with `HttpOnly`, `Secure`, `SameSite=Lax`
   (browser-session cookie, sliding server-side expiry
   `STACKVAULT_SESSION_TTL`; the Status tab's automatic refresh does not
   extend it); a new session id on every login; logout and
   password change end sessions (password change: all other sessions).
   Login rate limiting per username: 5 free failures, then exponential
   lockout up to 15 minutes (in memory). Unknown usernames cost the same
   time as wrong passwords. Login is refused over plain HTTP (no TLS and no
   `X-Forwarded-Proto: https`) unless `STACKVAULT_ALLOW_INSECURE_HTTP=true`.
   Passwords: 12 to 256 characters.
   **Initial admin:** without an active admin, user `admin` is created with
   a random password (100 bits), printed as a banner in the container log.
   It must be changed at the first login (all other pages redirect to the
   change form). Until it is changed, every start generates a new one, so
   the plaintext is never stored; afterwards nothing is printed.
7. **CSRF.** Two layers: Go's `http.CrossOriginProtection` rejects
   cross-site browser requests (Sec-Fetch-Site/Origin), and every
   state-changing request of a logged-in user needs the per-session token
   (form field `csrf_token` or header `X-CSRF-Token`). Pages are served
   with `Cache-Control: no-store` and a strict CSP (`default-src 'self'`).
8. **Authorization.** Checked per request and per stack (see data-model.md);
   deny by default. The rule is one function (`auth.Allowed`); handlers
   load the stack via `loadStack`, which validates the name and answers
   404 for stacks the user may not view. Admin-only routes are grouped
   behind `requireAdmin`. Admins cannot change their own flags in the UI,
   and the store refuses changes that leave no active admin.
9. **Audit log.** Login, failed login, job creation (manual and scheduled),
   restore, permission, schedule and repository changes. No secrets in entries.
10. **TLS.** The app speaks plain HTTP and is meant to run behind a reverse
    proxy that terminates TLS; the README says so prominently. Login over
    plain HTTP is refused unless `STACKVAULT_ALLOW_INSECURE_HTTP=true`.
11. **Restore safety.** Restore takes a `pre-restore` safety snapshot (only
    admins can skip it) and by default swaps directories, so the current
    directory is untouched until the restored copy is complete. The
    in-place restore (any user with the restore right, chosen explicitly)
    overwrites the stack directory; its only way back is the safety
    snapshot, and a failed in-place restore leaves the stack stopped
    instead of starting a partly restored one. Automatic rollbacks always
    swap.
12. **Backup of the backup tool.** The manager's `/data` (DB, job files) is
    inside its stack folder and gets backed up with it. With the default key
    source, `/data/secret.key` is in that backup too, so a restore of the
    manager's stack brings back a matching key and database; the backup is
    protected by the restic repository password. Users who want the key in
    no backup at all set `STACKVAULT_SECRET_KEY_FILE` to a file outside
    `STACKS_ROOT` (optional hardening, documented in the README). Either
    way the README tells users to keep a copy of the key elsewhere.

13. **Container shell.** Off unless the operator sets
    `STACKVAULT_ENABLE_SHELL=true`; then the links and routes exist for admins
    only (otherwise they do not exist at all). A session needs an admin
    session, the CSRF token and a same-origin WebSocket upgrade, and targets
    only a running container from the stack's own container list (after the
    compose project owner check), by validated id. The manager runs `docker
    exec -it` with a fixed argument list (no shell syntax on our side; the
    shell is probed with `test -x`). Sessions end within 30 s after the
    login becomes invalid. Start and end are audited; keystrokes are not
    recorded (they may contain secrets). At most 2 sessions per admin, 10
    in total.

## Reporting vulnerabilities

See `SECURITY.md`: reports go through GitHub private vulnerability
reporting (must be enabled in the repository settings).
