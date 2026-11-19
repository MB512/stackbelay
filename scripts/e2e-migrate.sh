#!/usr/bin/env bash
# End-to-end test of moving a stack to another host (issue #1) with the
# worker, the local Docker daemon and a local restic repository shared by
# both "hosts": host A backs the stack up, host B imports it (first
# restore creates the directory), syncs it with a newer snapshot,
# and takes over after A's final backup.
#
# Both hosts share one Docker daemon here, and compose identifies a stack
# by its project name (the directory name). So A's containers are taken
# down before B touches the stack, as on a real move ("stop on the source
# host"); otherwise B's project check refuses the stack, rightly.
#
# Requirements: docker (with compose plugin) usable without sudo, restic >= 0.17, go.
# Usage: scripts/e2e-migrate.sh          (KEEP=1 keeps the temp directory)
set -euo pipefail

cd "$(dirname "$0")/.."
WORK="$(mktemp -d "${TMPDIR:-/tmp}/stackvault-e2e-migrate.XXXXXX")"
ROOT_A="$WORK/host-a/stacks"
ROOT_B="$WORK/host-b/srv" # another path: only the stack name must match
STACK=e2e-move
DIR_A="$ROOT_A/$STACK"
DIR_B="$ROOT_B/$STACK"
PROJECT="$STACK"

pass() { printf '  \033[32mok\033[0m   %s\n' "$*"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$*"; exit 1; }
step() { printf '\n== %s\n' "$*"; }

cleanup() {
	if [[ "${KEEP:-0}" == 1 ]]; then
		echo "kept $WORK and project $PROJECT (remove: docker compose -p $PROJECT down; rm -rf $WORK)"
		return
	fi
	docker compose --project-name "$PROJECT" down --remove-orphans >/dev/null 2>&1 || true
	rm -rf "$WORK"
}
trap cleanup EXIT

# Value of a JSON field from the worker log line with the given msg.
log_field() { # file msg field
	python3 -I -c '
import json, sys
for line in open(sys.argv[1]):
    try: rec = json.loads(line)
    except ValueError: continue
    if rec.get("msg") == sys.argv[2]: print(rec.get(sys.argv[3], ""))
' "$1" "$2" "$3"
}

# worker on host A or B: separate stacks roots and host names, one repository.
on_a() { STACKS_ROOT="$ROOT_A" STACKVAULT_HOSTNAME=host-a ./bin/worker "$@"; }
on_b() { STACKS_ROOT="$ROOT_B" STACKVAULT_HOSTNAME=host-b ./bin/worker "$@"; }

backup_a() { # value -> snapshot id; A runs, is backed up, then taken down
	echo "$1" >"$DIR_A/data/value.txt"
	(cd "$DIR_A" && docker compose up --detach --quiet-pull --wait app >/dev/null)
	on_a backup "$STACK" 2>"$WORK/backup-$1.log" || { cat "$WORK/backup-$1.log"; fail "backup on A failed"; }
	(cd "$DIR_A" && docker compose down >/dev/null 2>&1)
	log_field "$WORK/backup-$1.log" "job succeeded" snapshot
}

step "build worker"
go build -o bin/worker ./cmd/worker
pass "bin/worker"

step "set up the stack on host A in $WORK"
mkdir -p "$DIR_A/data" "$ROOT_B"
echo "kept across syncs" >"$DIR_A/data/static.txt"
cat >"$DIR_A/compose.yml" <<'EOF'
services:
  app:
    image: busybox:1.36
    command: ["sh", "-c", "cat /data/value.txt; exec sleep 3600"]
    volumes: ["./data:/data:ro"]
EOF
printf 'e2e-test-password\n' >"$WORK/pw"
export RESTIC_REPOSITORY="$WORK/repo" RESTIC_PASSWORD_FILE="$WORK/pw" STACKVAULT_JOB_TIMEOUT=5m
restic init --quiet
SNAP1="$(backup_a v1)"
[[ -n "$SNAP1" ]] || fail "no snapshot id"
pass "host A backed up v1 ($SNAP1)"

step "host B imports the stack (first restore creates the directory, not started)"
on_b restore --import --no-start --limit-download 102400 "$STACK" "${SNAP1:0:8}" 2>"$WORK/import.log" ||
	{ cat "$WORK/import.log"; fail "import failed"; }
[[ "$(cat "$DIR_B/data/value.txt")" == v1 ]] || fail "imported data wrong"
[[ ! -e "$DIR_B/.stackvault-restore-incomplete" ]] || fail "incomplete-restore marker left after a complete import"
grep -q '"msg":"stopping stack"' "$WORK/import.log" && fail "an import stopped something"
grep -q 'pre-restore' "$WORK/import.log" && fail "an import took a safety snapshot"
[[ -z "$(docker compose --project-name "$PROJECT" ps --quiet)" ]] || fail "imported stack was started"
pass "directory created at $DIR_B with v1, nothing started"

step "an import never touches an existing directory"
if on_b restore --import "$STACK" "${SNAP1:0:8}" 2>"$WORK/again.log"; then fail "second import succeeded"; fi
grep -q 'already exists' "$WORK/again.log" || { cat "$WORK/again.log"; fail "unexpected error"; }
[[ "$(cat "$DIR_B/data/value.txt")" == v1 ]] || fail "existing directory changed"
pass "refused"

step "host A backs up v2, host B syncs in place (no safety snapshot, not started)"
SNAP2="$(backup_a v2)"
static_inode="$(stat -c %i "$DIR_B/data/static.txt")"
on_b restore --no-start "$STACK" "${SNAP2:0:8}" 2>"$WORK/sync.log" ||
	{ cat "$WORK/sync.log"; fail "sync failed"; }
[[ "$(cat "$DIR_B/data/value.txt")" == v2 ]] || fail "sync did not bring v2"
[[ "$(stat -c %i "$DIR_B/data/static.txt")" == "$static_inode" ]] || fail "unchanged file was rewritten"
[[ -z "$(docker compose --project-name "$PROJECT" ps --quiet)" ]] || fail "synced stack was started"
pass "v2 synced in place, unchanged file kept"

step "final backup on A (stopped), final restore on B with start and pinned images"
SNAP3="$(backup_a v3)"
on_b restore --pin-images "$STACK" "${SNAP3:0:8}" 2>"$WORK/final.log" ||
	{ cat "$WORK/final.log"; fail "final restore failed"; }
SEEN="$(docker compose --project-directory "$DIR_B" exec -T app cat /data/value.txt)"
[[ "$SEEN" == v3 ]] || fail "app on B sees '$SEEN'"
WORKDIR="$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project.working_dir"}}' "$PROJECT-app-1")"
[[ "$WORKDIR" == "$DIR_B" ]] || fail "container belongs to $WORKDIR"
pass "stack runs on B from $DIR_B with v3"

step "backups on B use B's host name"
on_b backup "$STACK" 2>"$WORK/backup-b.log" || { cat "$WORK/backup-b.log"; fail "backup on B failed"; }
HOSTS="$(restic snapshots --json | python3 -I -c 'import json,sys; print(" ".join(sorted({s["hostname"] for s in json.load(sys.stdin)})))')"
[[ "$HOSTS" == "host-a host-b" ]] || fail "hosts in the repository: $HOSTS"
pass "repository holds snapshots of host-a and host-b"

printf '\n\033[32mall migration checks passed\033[0m\n'
