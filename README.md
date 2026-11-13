# StackVault

Self-hosted web app (Go) that backs up Docker Compose stacks from a host to
[restic](https://restic.net) repositories.

Status: early design phase. See `docs/` for architecture, data model and
security notes, and `CLAUDE.md` for the development guidelines.

## Planned features

- Multi-user with per-stack permissions (who may back up / restore)
- One restic repository per stack
- Scheduled and manual backups
- Backup pipeline: metadata -> optional pre-stop snapshot -> stop -> snapshot -> start
- Backup and restore run in a separate worker container (self-backup/restore possible)
- Snapshot cache, live logs, stack migration between hosts, image pinning on restore

## License

[MIT](LICENSE)
