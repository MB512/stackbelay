# Changelog

All notable changes to StackBelay. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), versions follow
[Semantic Versioning](https://semver.org/).

## [Unreleased]

## [0.1.0] - 2026-11-24

First release. StackBelay backs up Docker Compose stacks to
[restic](https://restic.net) repositories, restores them and updates them
with a backup and automatic rollback. See the README for the setup.

### Added

- **Backups per stack**: `metadata.txt` (image digests, service states),
  a snapshot while the stack runs, `compose stop`, the final snapshot,
  `compose start`. The stack is started again in every case. Only one
  stack is stopped at a time (downtime slots); backups of several stacks
  run side by side.
- **Repositories** managed by admins in one place and shared by stacks
  (#39): a change applies to every stack that uses it; "Initialize
  repository" for a new location, e.g. per year. The form has a type
  (REST server, S3, other) with its own fields, shows values that are not
  secret and masks stored secrets (#45).
- **Restores** in place after a safety snapshot; an interrupted restore
  continues where it stopped. Optionally with the image versions from
  backup time (pinned by digest), and optionally with the tag for a
  service whose version the registry no longer has (#12).
- **Moving stacks between hosts** through their repository (Import
  stack); the setup of a stack whose directory is gone can be forgotten
  so its name can be imported cleanly (#38).
- **Updates** with backup and automatic rollback, an update check without
  pulling, and no changes at all when no image is new. Image references
  are compared the way Docker does (#40).
- **Schedules** (cron) for backups, updates and host cleanups, a job queue
  with one job per stack at a time, manual runs.
- **Job progress**: phases, restic transfer, image pulls, the steps of
  `compose up` (#28), restic's own messages in the job log.
- **Stack list, status page, live container logs** (per stack and
  global), up/start/restart/stop/down, compose editor, opt-in container
  shell for admins, host cleanup with preview.
- **Users** with per-stack rights (backup, restore, update, operate,
  logs), audit log, admin pages for jobs, repositories and the host.
- Workers in separate containers, so StackBelay backs up, restores and
  updates its own stack.
- Only creating and reading restic operations: works with an append-only
  rest-server. Retention is left to the repository owner, e.g. with
  [BelayPoint](https://github.com/MB512/belaypoint).

[Unreleased]: https://github.com/MB512/stackbelay/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/MB512/stackbelay/releases/tag/v0.1.0
