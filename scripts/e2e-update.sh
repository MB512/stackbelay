#!/usr/bin/env bash
# End-to-end test of the update job against the local Docker daemon and a
# local restic repository (worker on the host, like scripts/e2e-local.sh):
#
#   1. update with backup succeeds (pull, pre-stop + final snapshot, up)
#   2. update with backup fails (service unhealthy) -> automatic rollback
#   3. update without backup fails -> no rollback, stack left as it is
#
# A failure is forced with a healthcheck that fails once the file "broken"
# exists in a named volume (not part of the stack directory, so neither the
# snapshot nor the rollback touches it).
#
# Requirements: docker (with compose plugin) usable without sudo, network
# access to Docker Hub (compose pull), restic >= 0.17, go.
# Usage: scripts/e2e-update.sh            (KEEP=1 keeps the temp directory)
set -euo pipefail

cd "$(dirname "$0")/.."
WORK="$(mktemp -d "${TMPDIR:-/tmp}/stackvault-e2e-update.XXXXXX")"
ROOT="$WORK/stacks"
STACK=e2e-update
DIR="$ROOT/$STACK"
PROJECT="$STACK"

pass() { printf '  \033[32mok\033[0m   %s\n' "$*"; }
fail() { printf '  \033[31mFAIL\033[0m %s\n' "$*"; exit 1; }
step() { printf '\n== %s\n' "$*"; }

cleanup() {
	if [[ "${KEEP:-0}" == 1 ]]; then
		echo "kept $WORK and project $PROJECT (remove: docker compose -p $PROJECT down -v; rm -rf $WORK)"
		return
	fi
	docker compose --project-name "$PROJECT" down --volumes --remove-orphans >/dev/null 2>&1 || true
	rm -rf "$WORK"
}
trap cleanup EXIT

state() { # service -> container state ("" if none)
	docker compose --project-directory "$DIR" ps --all --format '{{.Service}} {{.State}}' |
		awk -v s="$1" '$1 == s { print $2 }'
}
container_id() { docker compose --project-directory "$DIR" ps --all --quiet app; }
has_msg() { grep -q "\"msg\":\"$2" "$1"; } # log file, start of msg
snapshots() { restic snapshots --json --tag "$1" | python3 -I -c 'import json,sys; print(len(json.load(sys.stdin)))'; }
set_broken() { docker run --rm -v "${PROJECT}_flags:/flags" busybox:1.36 sh -c "$1"; }

step "build worker"
go build -o bin/worker ./cmd/worker
pass "bin/worker"

step "set up stack and repository in $WORK"
mkdir -p "$DIR/data"
echo v1 >"$DIR/data/value.txt"
cat >"$DIR/compose.yml" <<'EOF'
services:
  app:
    image: busybox:1.36
    command: ["sh", "-c", "exec sleep 3600"]
    volumes: ["./data:/data", "flags:/flags"]
    healthcheck:
      test: ["CMD", "sh", "-c", "test ! -f /flags/broken"]
      interval: 1s
      retries: 2
volumes:
  flags: {}
EOF
printf 'e2e-test-password\n' >"$WORK/pw"
export STACKS_ROOT="$ROOT" STACKVAULT_HOSTNAME=e2e-host STACKVAULT_JOB_TIMEOUT=10m
REPO_ENV=(RESTIC_REPOSITORY="$WORK/repo" RESTIC_PASSWORD_FILE="$WORK/pw")
env "${REPO_ENV[@]}" restic init --quiet
(cd "$DIR" && docker compose up --detach --quiet-pull --wait app >/dev/null)
[[ "$(state app)" == running ]] || fail "app not running"
pass "app running"

step "1. update with backup succeeds"
before="$(container_id)"
env "${REPO_ENV[@]}" bin/worker update "$STACK" 2>"$WORK/update1.log" || { cat "$WORK/update1.log"; fail "update failed"; }
[[ "$(state app)" == running ]] || fail "app not running after update"
[[ "$(container_id)" != "$before" ]] || fail "container was not recreated"
pass "app recreated and running"
export RESTIC_REPOSITORY="$WORK/repo" RESTIC_PASSWORD_FILE="$WORK/pw"
[[ "$(snapshots pre-stop)" == 1 && "$(snapshots final)" == 1 ]] || fail "expected one pre-stop and one final snapshot"
unset RESTIC_REPOSITORY RESTIC_PASSWORD_FILE
pass "pre-stop and final snapshot taken"

step "2. update with backup fails: automatic rollback"
echo v2 >"$DIR/data/value.txt" # the state the rollback must return to
set_broken "touch /flags/broken"
if env "${REPO_ENV[@]}" bin/worker update "$STACK" 2>"$WORK/update2.log"; then
	cat "$WORK/update2.log"
	fail "update succeeded although the service is unhealthy"
fi
has_msg "$WORK/update2.log" "rolled back to the state before the update" || { cat "$WORK/update2.log"; fail "no rollback"; }
old=("$ROOT/$STACK".old-*)
[[ -d "${old[0]}" ]] || fail "failed version directory not kept"
[[ "$(cat "$DIR/data/value.txt")" == v2 ]] || fail "stack data after rollback: $(cat "$DIR/data/value.txt")"
has_msg "$WORK/update2.log" "image pinned" || fail "rollback did not pin the previous image"
pass "rolled back (data v2, image pinned, failed version kept at ${old[0]##*/})"
rm -rf "${old[@]}"

step "3. update without backup fails: no rollback"
set_broken "touch /flags/broken"
before="$(container_id)"
if bin/worker update --no-backup "$STACK" 2>"$WORK/update3.log"; then
	fail "update succeeded although the service is unhealthy"
fi
has_msg "$WORK/update3.log" "NOT ROLLED BACK" || { cat "$WORK/update3.log"; fail "missing no-rollback message"; }
! has_msg "$WORK/update3.log" "rolling back" || fail "rolled back without backup"
compgen -G "$ROOT/$STACK.old-*" >/dev/null && fail "a directory was swapped without backup"
[[ "$(container_id)" != "$before" ]] || fail "update without backup did not recreate the container"
pass "left as it is (new container kept, no restore)"

# Without repository settings, only --no-backup may run.
if bin/worker update "$STACK" 2>"$WORK/update4.log"; then fail "update with backup ran without repository"; fi
grep -q "no repository configured" "$WORK/update4.log" || fail "unexpected error: $(tail -1 "$WORK/update4.log")"
pass "update with backup refused without repository settings"

printf '\n\033[32mall update checks passed\033[0m\n'
