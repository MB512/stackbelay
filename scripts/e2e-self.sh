#!/usr/bin/env bash
# End-to-end test of self-backup and self-restore: the manager runs in a
# container as a stack under STACKS_ROOT (its database and job files are part
# of that stack) and backs up and restores itself through its own UI.
#
# Checks that after the self-restore the manager runs with the database from
# the snapshot, knows the restore job (its job files are excluded from the
# restore and reconciled) and shows
# the self-backup with its real result.
#
# Requirements: docker (with compose plugin) usable without sudo, go, curl,
# openssl. Needs Docker Engine (not Docker Desktop's WSL integration, see
# docs/architecture.md). Pulls golang:1.26-alpine, docker:29-cli and busybox:1.36.
# Usage: scripts/e2e-self.sh     (KEEP=1 keeps everything, PORT=18100 sets the port)
set -euo pipefail

cd "$(dirname "$0")/.."
WORK="$(mktemp -d "${TMPDIR:-/tmp}/stackbelay-e2e-self.XXXXXX")"
ROOT="$WORK/stacks"
# The stack name is the directory name and, as for every stack, also the
# compose project name (StackBelay runs compose like a user in that
# directory). It must not collide with a real StackBelay stack on this host.
NAME=e2eself-manager
SELF="$ROOT/$NAME"
REPO="$ROOT/_repo/$NAME" # not a stack (no compose file)
IMAGE=stackbelay-e2e:latest
PORT="${PORT:-18100}"
B="http://127.0.0.1:$PORT"
JAR="$WORK/cookies"
PROJECT="$NAME"
CONTAINER="$NAME-manager-1"

pass() { printf '  \033[32mok\033[0m   %s\n' "$*"; }
fail() {
	printf '  \033[31mFAIL\033[0m %s\n' "$*"
	echo "manager log: docker logs $CONTAINER (run with KEEP=1 to keep it)"
	exit 1
}
step() { printf '\n== %s\n' "$*"; }

compose() { docker compose --project-directory "$SELF" "$@"; }
cleanup() {
	if [[ "${KEEP:-0}" == 1 ]]; then
		echo "kept $WORK and project $PROJECT (remove: docker compose -p $PROJECT down; then delete $WORK, some files belong to root)"
		return
	fi
	compose down >/dev/null 2>&1 || true
	for id in $(job_ids); do
		docker rm -f "stackbelay-job-$(tr 'A-Z' 'a-z' <<<"$id")" >/dev/null 2>&1 || true
	done
	docker run --rm -v "$(dirname "$WORK"):/p" busybox:1.36 rm -rf "/p/$(basename "$WORK")" >/dev/null 2>&1 || rm -rf "$WORK"
}
trap cleanup EXIT

c() { curl -sS -c "$JAR" -b "$JAR" "$@"; }
# job_ids: ids of all job directories (they belong to root: list them in a container).
job_ids() {
	docker run --rm -v "$ROOT:/r:ro" busybox:1.36 sh -c "ls /r/$NAME/data/jobs 2>/dev/null" |
		grep -E '^[0-9A-Z]{26}$' | sort -u || true
}
# job_state <job path>: state badge of the job page ("" while the manager is down).
job_state() { c "$B$1" 2>/dev/null | grep -o 'badge [a-z]*">[a-z]*' | head -1 | sed 's/.*>//' || true; }
wait_job() { # <job path> -> final state; the manager may be down in between
	local s
	for _ in $(seq 1 150); do
		s="$(job_state "$1")"
		case "$s" in succeeded | failed | cancelled) echo "$s"; return ;; esac
		sleep 2
	done
	echo timeout
}
wait_healthy() {
	for _ in $(seq 1 60); do curl -sf "$B/healthz" >/dev/null 2>&1 && return; sleep 1; done
	fail "manager not healthy"
}
csrf() { c "$B$1" | grep -o 'csrf_token" value="[^"]*' | head -1 | cut -d'"' -f3; }

step "build image"
docker build --quiet -f deploy/Dockerfile --build-arg VERSION=e2e -t "$IMAGE" . >/dev/null
pass "$IMAGE"

step "manager as its own stack"
mkdir -p "$SELF/data" "$ROOT/_repo"
openssl rand -base64 32 >"$WORK/secret.key"
cat >"$SELF/compose.yml" <<YML
services:
  manager:
    image: $IMAGE
    environment:
      STACKS_ROOT: $ROOT
      STACKBELAY_HOSTNAME: e2e-self
      STACKBELAY_SECRET_KEY_FILE: /run/secrets/key
      STACKBELAY_ALLOW_INSECURE_HTTP: "true"
    ports: ["127.0.0.1:$PORT:8080"]
    volumes:
      - $ROOT:$ROOT
      - /var/run/docker.sock:/var/run/docker.sock
      - ./data:/data
      - $WORK/secret.key:/run/secrets/key:ro
YML
compose up --detach --quiet-pull >/dev/null 2>&1
wait_healthy
PW="$(docker logs "$CONTAINER" 2>&1 | grep -o 'INITIAL ADMIN PASSWORD (user "admin"): [^ ]*' | awk '{print $NF}')"
[[ -n "$PW" ]] || fail "no initial admin password"
c -o /dev/null -d "username=admin" --data-urlencode "password=$PW" "$B/login"
T="$(csrf /account/password)"
c -o /dev/null --data-urlencode "csrf_token=$T" --data-urlencode "current=$PW" \
	-d "new=e2e+long+password&confirm=e2e+long+password" "$B/account/password"
c "$B/" | grep -q "href=\"/stacks/$NAME\"" || fail "own stack not discovered"
pass "manager running in its stack, logged in"
c --data-urlencode "csrf_token=$T" --data-urlencode "location=$REPO" -d "password=self-repo-pw" \
	"$B/stacks/$NAME/repository" | grep -q 'Initialize repository</button>' || fail "init not offered"
c --data-urlencode "csrf_token=$T" "$B/stacks/$NAME/repository/init" | grep -q 'Repository initialized' || fail "init failed"
pass "repository initialized"

step "self-backup"
BACKUP="$(c -o /dev/null -w '%{redirect_url}' --data-urlencode "csrf_token=$T" "$B/stacks/$NAME/backup")"
BACKUP="${BACKUP#"$B"}"
S="$(wait_job "$BACKUP")"
[[ "$S" == succeeded ]] || fail "self-backup: $S"
pass "self-backup succeeded ($BACKUP); manager restarted, session kept"

step "change after the backup: create user 'later'"
c -o /dev/null --data-urlencode "csrf_token=$T" -d "username=later" "$B/admin/users"
c "$B/admin/users" | grep -q '<td>later' || fail "user not created"
pass "user 'later' exists"

step "self-restore of the backup"
sleep 10 # snapshot refresh cooldown
c -o /dev/null --data-urlencode "csrf_token=$T" "$B/stacks/$NAME/snapshots/refresh"
SNAP="$(c "$B/stacks/$NAME/backups" | grep -o 'restore?snapshot=[0-9a-f]*' | head -1 | cut -d= -f2 || true)"
[[ -n "$SNAP" ]] || fail "no snapshot to restore"
RESTORE="$(c -o /dev/null -w '%{redirect_url}' --data-urlencode "csrf_token=$T" -d "snapshot_id=$SNAP" \
	-d safety_snapshot=on -d start=on "$B/stacks/$NAME/restore")"
RESTORE="${RESTORE#"$B"}"
S="$(wait_job "$RESTORE")"
[[ "$S" == succeeded ]] || fail "self-restore: '$S' (job page after restart: $(c -o /dev/null -w '%{http_code}' "$B$RESTORE"))"
pass "self-restore succeeded and is known to the restarted manager ($RESTORE)"

step "state after the restore"
wait_healthy
c "$B/admin/users" | grep -q '<td>later' && fail "user 'later' still exists: database not restored"
pass "database is the one from the snapshot (user 'later' gone)"
S="$(job_state "$BACKUP")"
[[ "$S" == succeeded ]] || fail "self-backup shown as '$S' after the restore"
pass "self-backup still shown as succeeded"
compgen -G "$ROOT/$NAME.*" >/dev/null && fail "a sibling directory was created"
[[ ! -e "$ROOT/$NAME/.stackbelay-restore-incomplete" ]] || fail "incomplete-restore marker left"
pass "restored in place (no sibling directories, no marker)"
sleep 5
IDS="$(job_ids)"
[[ "$(wc -w <<<"$IDS")" -ge 2 ]] || fail "expected at least 2 job directories, found: $IDS"
for id in $IDS; do
	docker container inspect "stackbelay-job-$(tr 'A-Z' 'a-z' <<<"$id")" >/dev/null 2>&1 &&
		fail "worker container of $id left"
done
pass "worker containers removed ($(wc -w <<<"$IDS") jobs checked)"
docker logs "$CONTAINER" 2>&1 | grep -q 'imported job from its job files' || fail "no job import in the manager log"
pass "manager imported the restore from the job files"

printf '\n\033[32mall self-backup/restore checks passed\033[0m\n'
