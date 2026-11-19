#!/usr/bin/env bash
# End-to-end smoke test of the worker against the local Docker daemon and a
# local restic repository. Creates a throwaway stack in a temp directory,
# backs it up, changes data, restores (in place, also an incomplete restore)
# and checks the result.
#
# Requirements: docker (with compose plugin) usable without sudo, restic >= 0.17, go.
# Usage: scripts/e2e-local.sh            (KEEP=1 keeps the temp directory)
set -euo pipefail

cd "$(dirname "$0")/.."
WORK="$(mktemp -d "${TMPDIR:-/tmp}/stackvault-e2e.XXXXXX")"
ROOT="$WORK/stacks"
STACK=e2e-web
DIR="$ROOT/$STACK"
PROJECT="$STACK" # compose project name = directory name

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

state() { # service -> container state ("" if none)
	docker compose --project-directory "$DIR" ps --all --format '{{.Service}} {{.State}}' |
		awk -v s="$1" '$1 == s { print $2 }'
}

step "build worker"
go build -o bin/worker ./cmd/worker
pass "bin/worker"

step "set up stack and repository in $WORK"
mkdir -p "$DIR/data"
chmod 750 "$DIR" # not the default, so the restore must bring it back
echo v1 >"$DIR/data/value.txt"
cat >"$DIR/compose.yml" <<'EOF'
services:
  app:
    image: busybox:1.36
    command: ["sh", "-c", "cat /data/value.txt; exec sleep 3600"]
    volumes: ["./data:/data:ro"]
    healthcheck:
      test: ["CMD", "test", "-f", "/data/value.txt"]
      interval: 1s
      retries: 10
  oneshot:
    image: busybox:1.36
    command: ["true"]
EOF
printf 'e2e-test-password\n' >"$WORK/pw"
export STACKS_ROOT="$ROOT" STACKVAULT_HOSTNAME=e2e-host \
	RESTIC_REPOSITORY="$WORK/repo" RESTIC_PASSWORD_FILE="$WORK/pw" \
	STACKVAULT_JOB_TIMEOUT=5m
restic init --quiet
(cd "$DIR" && docker compose up --detach --quiet-pull --wait app >/dev/null)
(cd "$DIR" && docker compose up --detach --quiet-pull oneshot >/dev/null)
sleep 2
[[ "$(state app)" == running ]] || fail "app not running"
[[ "$(state oneshot)" == exited ]] || fail "oneshot not exited: $(state oneshot)"
pass "app running, oneshot exited"

step "backup with pre-stop snapshot"
./bin/worker backup --pre-stop "$STACK" 2>"$WORK/backup.log" || { cat "$WORK/backup.log"; fail "backup failed"; }
SNAP="$(log_field "$WORK/backup.log" "job succeeded" snapshot)"
PRESTOP="$(log_field "$WORK/backup.log" "job succeeded" pre_stop_snapshot)"
[[ -n "$SNAP" && -n "$PRESTOP" ]] || fail "no snapshot ids in log"
pass "final $SNAP, pre-stop $PRESTOP"
[[ "$(state app)" == running ]] || fail "app not restarted after backup"
[[ "$(state oneshot)" == exited ]] || fail "oneshot was started although it was not running"
pass "app restarted, oneshot left alone"
grep -q '^# stackvault-metadata v1$' "$DIR/metadata.txt" || fail "metadata.txt missing"
grep -qP '^app\trunning\tbusybox:1.36\tbusybox@sha256:[0-9a-f]{64}\tsha256:' "$DIR/metadata.txt" ||
	{ cat "$DIR/metadata.txt"; fail "metadata.txt has no digest for app"; }
pass "metadata.txt records app with repo digest"
PARENT="$(restic snapshots --json "$SNAP" | python3 -I -c 'import json,sys; s=json.load(sys.stdin)[0]; print(s.get("parent",""), ",".join(s["tags"]), s["hostname"], s["paths"][0])')"
[[ "$PARENT" == "$PRESTOP"* && "$PARENT" == *" final e2e-host $DIR" ]] || fail "unexpected final snapshot: $PARENT"
pass "final snapshot: parent=pre-stop, tag final, host e2e-host, path $DIR"

step "backup via job directory (worker run)"
JOB="$WORK/job"
mkdir -p "$JOB"
printf '{"version":1,"id":"01JCZ8G3T4S5V6W7X8Y9Z0ABCD","type":"backup","stack":"%s"}\n' "$STACK" >"$JOB/spec.json"
# Password via environment here, to check that it is redacted from the log.
env -u RESTIC_PASSWORD_FILE RESTIC_PASSWORD="$(cat "$WORK/pw")" ./bin/worker run "$JOB" 2>/dev/null ||
	{ cat "$JOB/log.txt"; fail "worker run failed"; }
python3 -I - "$JOB/status.json" <<'PY' || fail "unexpected status.json: $(cat "$JOB/status.json")"
import json, sys
s = json.load(open(sys.argv[1]))
assert s["state"] == "succeeded" and s["snapshot"] and s["started_at"] and s["finished_at"], s
PY
pass "status.json: succeeded with snapshot"
grep -q '"msg":"job succeeded"' "$JOB/log.txt" || fail "log.txt incomplete"
grep -q "$(cat "$WORK/pw")" "$JOB/log.txt" && fail "repository password in log.txt"
pass "log.txt written as JSON lines, password not in log"
[[ "$(state app)" == running ]] || fail "app not restarted after job"
pass "app restarted"

step "change data, then restore (in place)"
echo v2 >"$DIR/data/value.txt"
echo stale >"$DIR/data/created-after-backup.txt"
before_inode="$(stat -c %i "$DIR")"
./bin/worker restore "$STACK" "${SNAP:0:8}" 2>"$WORK/restore.log" || { cat "$WORK/restore.log"; fail "restore failed"; }
SAFETY="$(log_field "$WORK/restore.log" "job succeeded" safety_snapshot)"
[[ "$(cat "$DIR/data/value.txt")" == v1 ]] || fail "data not restored"
pass "data restored (v1)"
[[ ! -e "$DIR/data/created-after-backup.txt" ]] || fail "file created after the backup not deleted"
[[ "$(stat -c %i "$DIR")" == "$before_inode" ]] || fail "stack directory was replaced instead of restored in place"
compgen -G "$DIR.*" >/dev/null && fail "a sibling directory was created: $(ls -d "$DIR".*)"
pass "restored in place (extra file deleted, same directory, no sibling directories)"
[[ "$(stat -c %a "$DIR")" == 750 ]] || fail "stack dir mode not restored: $(stat -c %a "$DIR")"
pass "stack directory mode restored (750)"
[[ ! -e "$DIR/.stackvault-restore-incomplete" ]] || fail "incomplete-restore marker left after a complete restore"
[[ -n "$SAFETY" ]] && restic snapshots --json --tag pre-restore | grep -q "$SAFETY" || fail "no pre-restore snapshot"
pass "pre-restore safety snapshot $SAFETY"
[[ "$(state app)" == running ]] || fail "app not running after restore"
# Read the file inside the container: the bind mount must show the
# restored data.
SEEN="$(docker compose --project-directory "$DIR" exec -T app cat /data/value.txt)"
[[ "$SEEN" == v1 ]] || fail "app sees '$SEEN' instead of restored data"
pass "app running and its bind mount shows restored data"
[[ "$(state oneshot)" != running ]] || fail "oneshot running after restore"
pass "oneshot not started (was not running at backup time)"

step "an incomplete restore blocks other jobs and is continued by the next restore"
# What a restore that stopped half-way leaves behind (timeout, network error).
echo v3 >"$DIR/data/value.txt"
docker compose --project-directory "$DIR" stop >/dev/null 2>&1
: >"$DIR/.stackvault-restore-incomplete"
if ./bin/worker backup "$STACK" 2>"$WORK/blocked.log"; then fail "backup of a partly restored stack succeeded"; fi
grep -q 'did not complete' "$WORK/blocked.log" || { cat "$WORK/blocked.log"; fail "unexpected error for the blocked backup"; }
if ./bin/worker start "$STACK" 2>"$WORK/blocked.log"; then fail "start of a partly restored stack succeeded"; fi
[[ "$(state app)" != running ]] || fail "partly restored stack was started"
pass "backup and start refused while the restore is incomplete"
./bin/worker restore "$STACK" "${SNAP:0:8}" 2>"$WORK/continue.log" || { cat "$WORK/continue.log"; fail "continuing restore failed"; }
[[ -z "$(log_field "$WORK/continue.log" "job succeeded" safety_snapshot)" ]] || fail "safety snapshot of a partly restored directory"
[[ "$(cat "$DIR/data/value.txt")" == v1 && ! -e "$DIR/.stackvault-restore-incomplete" ]] || fail "restore not continued"
[[ "$(state app)" == running ]] || fail "app not running after the continued restore"
pass "restore continued (no safety snapshot, marker removed, app running)"

step "restore with an invalid snapshot is rejected before stopping"
if ./bin/worker restore "$STACK" deadbeef 2>"$WORK/bad.log"; then fail "restore of unknown snapshot succeeded"; fi
grep -q '"msg":"stopping stack"' "$WORK/bad.log" && fail "stack was stopped for an invalid snapshot"
[[ "$(state app)" == running ]] || fail "app not running"
pass "rejected, stack untouched"

printf '\n\033[32mall end-to-end checks passed\033[0m\n'
