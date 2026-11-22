#!/usr/bin/env bash
# End-to-end test of the update job against the local Docker daemon and a
# local restic repository (worker on the host, like scripts/e2e-local.sh):
#
#   1. update with backup succeeds (pull, pre-stop + final snapshot, up)
#   2. update with backup fails (service unhealthy) -> automatic rollback
#      (in place)
#   1b. update without a new image -> nothing touched
#   3. update without backup fails -> no rollback, stack left as it is
#   4. compose-apply (compose editor job) with a valid change
#   5. compose-apply with an image that does not exist -> rollback to the
#      previous compose file
#
# A failure is forced with a healthcheck that fails once the file "broken"
# exists in a named volume (not part of the stack directory, so neither the
# snapshot nor the rollback touches it). An update only acts when a running
# service gets a new image; pin_app makes that so without a registry by
# running the app pinned by digest, as a rollback does.
#
# Requirements: docker (with compose plugin) usable without sudo, network
# access to Docker Hub (compose pull), restic >= 0.17, go.
# Usage: scripts/e2e-update.sh            (KEEP=1 keeps the temp directory)
set -euo pipefail

cd "$(dirname "$0")/.."
WORK="$(mktemp -d "${TMPDIR:-/tmp}/stackbelay-e2e-update.XXXXXX")"
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
pin_app() {
	local digest
	digest="$(docker image inspect --format '{{index .RepoDigests 0}}' busybox:1.36)"
	printf 'services:\n  app:\n    image: %s\n' "$digest" >"$WORK/pin.yml"
	docker compose --project-directory "$DIR" --file "$DIR/compose.yml" --file "$WORK/pin.yml" \
		up --detach --quiet-pull --wait app >/dev/null
}
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
pin_app
before="$(container_id)"
env "${REPO_ENV[@]}" bin/worker update "$STACK" 2>"$WORK/update1.log" || { cat "$WORK/update1.log"; fail "update failed"; }
[[ "$(state app)" == running ]] || fail "app not running after update"
[[ "$(container_id)" != "$before" ]] || fail "container was not recreated"
pass "app recreated and running"
export RESTIC_REPOSITORY="$WORK/repo" RESTIC_PASSWORD_FILE="$WORK/pw"
[[ "$(snapshots pre-stop)" == 1 && "$(snapshots final)" == 1 ]] || fail "expected one pre-stop and one final snapshot"
unset RESTIC_REPOSITORY RESTIC_PASSWORD_FILE
pass "pre-stop and final snapshot taken"

step "1b. update again: no new image, nothing touched"
before="$(container_id)"
env "${REPO_ENV[@]}" bin/worker update "$STACK" 2>"$WORK/update1b.log" || { cat "$WORK/update1b.log"; fail "update failed"; }
has_msg "$WORK/update1b.log" "nothing to change" || { cat "$WORK/update1b.log"; fail "no 'nothing to change'"; }
[[ "$(container_id)" == "$before" ]] || fail "container recreated although nothing was new"
export RESTIC_REPOSITORY="$WORK/repo" RESTIC_PASSWORD_FILE="$WORK/pw"
[[ "$(snapshots pre-stop)" == 1 ]] || fail "snapshot taken although nothing was new"
unset RESTIC_REPOSITORY RESTIC_PASSWORD_FILE
pass "pulled, compared, nothing backed up or recreated"

step "2. update with backup fails: automatic rollback (in place)"
pin_app
echo v2 >"$DIR/data/value.txt" # the state the rollback must return to
inode="$(stat -c %i "$DIR")"
set_broken "touch /flags/broken"
if env "${REPO_ENV[@]}" bin/worker update "$STACK" 2>"$WORK/update2.log"; then
	cat "$WORK/update2.log"
	fail "update succeeded although the service is unhealthy"
fi
has_msg "$WORK/update2.log" "rolled back to the state before the update" || { cat "$WORK/update2.log"; fail "no rollback"; }
[[ "$(cat "$DIR/data/value.txt")" == v2 ]] || fail "stack data after rollback: $(cat "$DIR/data/value.txt")"
has_msg "$WORK/update2.log" "image pinned" || fail "rollback did not pin the previous image"
[[ "$(stat -c %i "$DIR")" == "$inode" ]] || fail "stack directory replaced instead of restored in place"
compgen -G "$ROOT/$STACK.*" >/dev/null && fail "a sibling directory was created by the rollback"
[[ ! -e "$DIR/.stackvault-restore-incomplete" ]] || fail "incomplete-restore marker left after the rollback"
pass "rolled back in place (data v2, image pinned, same directory)"

step "3. update without backup fails: no rollback"
set_broken "touch /flags/broken"
before="$(container_id)"
if bin/worker update --no-backup "$STACK" 2>"$WORK/update3.log"; then
	fail "update succeeded although the service is unhealthy"
fi
has_msg "$WORK/update3.log" "NOT ROLLED BACK" || { cat "$WORK/update3.log"; fail "missing no-rollback message"; }
! has_msg "$WORK/update3.log" "rolling back" || fail "rolled back without backup"
compgen -G "$ROOT/$STACK.*" >/dev/null && fail "a sibling directory was created without backup"
[[ "$(container_id)" != "$before" ]] || fail "update without backup did not recreate the container"
pass "left as it is (new container kept, no restore)"

# Without repository settings, only --no-backup may run.
if bin/worker update "$STACK" 2>"$WORK/update4.log"; then fail "update with backup ran without repository"; fi
grep -q "no repository configured" "$WORK/update4.log" || fail "unexpected error: $(tail -1 "$WORK/update4.log")"
pass "update with backup refused without repository settings"

# compose_job <job-dir> <new content file>: a compose-apply job directory as
# the manager writes it (spec.json + compose.new).
compose_job() {
	mkdir -p "$1"
	cp "$2" "$1/compose.new"
	python3 -I - "$1" "$DIR/compose.yml" "$STACK" <<'PY'
import hashlib, json, sys
job, current, stack = sys.argv[1:]
sha = lambda p: hashlib.sha256(open(p, "rb").read()).hexdigest()
spec = {"version": 1, "id": "01JCZ8G3T4S5V6W7X8Y9Z0ABCD", "type": "compose-apply", "stack": stack,
        "compose_sha256": sha(job + "/compose.new"), "compose_base_sha256": sha(current)}
json.dump(spec, open(job + "/spec.json", "w"))
PY
	# No manager here: grant the downtime slot in advance (issue #36).
	: >"$1/downtime-granted"
}
job_state() { python3 -I -c 'import json,sys; print(json.load(open(sys.argv[1]))["state"])' "$1/status.json"; }

step "4. compose-apply with a valid change"
set_broken "rm -f /flags/broken"
(cd "$DIR" && docker compose up --detach --wait app >/dev/null)
cp "$DIR/compose.yml" "$WORK/compose.orig"
sed 's/exec sleep 3600/echo edited; exec sleep 3600/' "$DIR/compose.yml" >"$WORK/compose.edited"
compose_job "$WORK/job4" "$WORK/compose.edited"
env "${REPO_ENV[@]}" bin/worker run "$WORK/job4" 2>/dev/null || { cat "$WORK/job4/log.txt"; fail "compose-apply failed"; }
[[ "$(job_state "$WORK/job4")" == succeeded ]] || fail "job state $(job_state "$WORK/job4")"
cmp -s "$DIR/compose.yml" "$WORK/compose.edited" || fail "compose file not replaced"
cmp -s "$WORK/job4/compose.previous" "$WORK/compose.orig" || fail "previous compose file not kept"
docker compose --project-directory "$DIR" logs app | grep -q edited || fail "app does not run the edited command"
[[ "$(state app)" == running ]] || fail "app not running"
pass "file replaced, previous kept, app runs the new configuration"

step "5. compose-apply with a missing image: rollback"
sed 's/busybox:1.36/busybox:stackvault-does-not-exist/' "$DIR/compose.yml" >"$WORK/compose.bad"
cp "$DIR/compose.yml" "$WORK/compose.before5"
compose_job "$WORK/job5" "$WORK/compose.bad"
if env "${REPO_ENV[@]}" bin/worker run "$WORK/job5" 2>/dev/null; then fail "compose-apply with a missing image succeeded"; fi
has_msg "$WORK/job5/log.txt" "rolled back to the state before the compose-apply" || { cat "$WORK/job5/log.txt"; fail "no rollback"; }
cmp -s "$DIR/compose.yml" "$WORK/compose.before5" || fail "compose file not rolled back"
[[ "$(state app)" == running ]] || fail "app not running after rollback"
python3 -I -c 'import json,sys; assert json.load(open(sys.argv[1]))["rolled_back"]' "$WORK/job5/status.json" || fail "status not rolled_back"
pass "rolled back to the previous compose file, app running"

printf '\n\033[32mall update checks passed\033[0m\n'
