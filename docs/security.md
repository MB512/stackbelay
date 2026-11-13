# Security

## Threat model (short)

Access to `/var/run/docker.sock` is equivalent to root on the host. The goal is
not to make that safe, but to ensure a compromised or malicious *web user*
cannot turn the app into a root shell.

Assets: host root, stack data, repository credentials, backups themselves.
Attackers considered: authenticated low-privilege users, unauthenticated network
attackers reaching the UI, a leaked database file.

## Rules

1. **No free-form execution.** Users can only trigger predefined job types
   (`backup`, `restore`) on discovered stacks. All external
   commands use `exec.CommandContext` with argument lists, never a shell.
2. **Input validation.** Stack names must match the discovered set. Snapshot IDs
   must match `^[0-9a-f]{8,64}$` and are verified against `restic snapshots`.
   Cron expressions are parsed and validated before saving.
3. **Docker access.** Only the manager spawns workers, from a fixed template
   (image, mounts, labels, command). Users cannot influence mounts, image,
   entrypoint or environment. Optional hardening: put a socket proxy between
   manager and Docker that allows only container create/start/inspect/logs and
   the calls the manager itself needs.
4. **Secrets at rest.** Repo passwords and backend credentials are encrypted
   with AES-256-GCM using `STACKVAULT_SECRET_KEY`. The key is not stored in the DB.
5. **Secrets in transit to worker.** Delivered via a tmpfs-mounted file or env
   of the worker container, removed after the job. Never CLI arguments (visible
   in `ps`), never logged. Log output from restic is filtered for known secret
   values.
6. **Authentication.** argon2id password hashes; random 256-bit session ids
   (stored hashed); cookies `HttpOnly`, `Secure`, `SameSite=Lax`; login rate
   limiting with backoff; session expiry and logout invalidation.
7. **CSRF.** Per-session token required on every state-changing request.
8. **Authorization.** Checked per request and per stack (see data-model.md);
   deny by default.
9. **Audit log.** Login, failed login, job creation, restore, permission and
   repository changes. No secrets in entries.
10. **TLS.** The app speaks plain HTTP and is meant to run behind a reverse
    proxy that terminates TLS. Document this prominently. Offer an option to
    refuse login over non-HTTPS unless explicitly allowed.
11. **Restore safety.** Restore always takes a `pre-restore` safety snapshot
    and swaps directories instead of overwriting in place.
12. **Backup of the backup tool.** The manager's `/data` (DB, job files) is
    inside its stack folder and gets backed up with it. The secrets key is NOT
    part of that backup by design; document where the user must keep it.

## Reporting vulnerabilities

Provide `SECURITY.md` in the repository with a private contact (GitHub private
vulnerability reporting) before the first release.
