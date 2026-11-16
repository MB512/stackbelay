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
