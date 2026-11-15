#!/usr/bin/env bash
# End-to-end test of the manager and its worker containers against the
# local Docker daemon: the manager runs on the host, every job runs in a
# worker container started by the manager. Everything is driven through the
# web UI (curl): initialize a repository, back up, restore, check the stack.
#
# Requirements: docker (with compose plugin) usable without sudo, restic,
# go, curl. Pulls docker:29-cli and busybox:1.36.
# Usage: scripts/e2e-dispatch.sh        (KEEP=1 keeps the temp directory,
#                                        PORT=18090 sets the manager port)
#
# Docker Desktop (WSL integration) loses the bind mounts of containers that
# the worker stopped (see docs/architecture.md); there the test stack has no
# bind mount and the in-container data check is skipped.
set -euo pipefail

cd "$(dirname "$0")/.."
WORK="$(mktemp -d "${TMPDIR:-/tmp}/stackvault-e2e-dispatch.XXXXXX")"
ROOT="$WORK/stacks"
STACK=e2edispatch
DIR="$ROOT/$STACK"
REPO="$ROOT/_repo/$STACK" # not a stack (no compose file); visible to manager and worker
IMAGE=stackvault-e2e:latest
PORT="${PORT:-18090}"
B="http://127.0.0.1:$PORT"
JAR="$WORK/cookies"
MANAGER_PID=""

pass() { printf '  \033[32mok\033[0m   %s\n' "$*"; }
fail() {
	printf '  \033[31mFAIL\033[0m %s\n' "$*"
	echo "manager log: $WORK/manager.log (run with KEEP=1 to keep it)"
	exit 1
}
step() { printf '\n== %s\n' "$*"; }

cleanup() {
	[[ -n "$MANAGER_PID" ]] && kill "$MANAGER_PID" 2>/dev/null || true
	if [[ "${KEEP:-0}" == 1 ]]; then
		echo "kept $WORK and project $STACK (remove: docker compose -p $STACK down; then delete $WORK, some files belong to root)"
		return
	fi
	docker compose --project-name "$STACK" down --remove-orphans >/dev/null 2>&1 || true
	# Only the workers of this test's jobs, never other jobs on this host.
	for job in "$WORK"/data/jobs/*/; do
		[[ -d "$job" ]] && docker rm -f "stackvault-job-$(basename "$job" | tr 'A-Z' 'a-z')" >/dev/null 2>&1
	done
	true
	# The worker runs as root: remove its files through a container.
	docker run --rm -v "$(dirname "$WORK"):/p" busybox:1.36 rm -rf "/p/$(basename "$WORK")" >/dev/null 2>&1 || rm -rf "$WORK"
}
trap cleanup EXIT

c() { curl -sS -c "$JAR" -b "$JAR" "$@"; }
state() { docker compose --project-directory "$DIR" ps --all --format '{{.Service}} {{.State}}' | awk -v s="$1" '$1 == s { print $2 }'; }
# job_state <job path>: state badge of the job page.
job_state() { c "$B$1" | grep -o 'badge [a-z]*">[a-z]*' | head -1 | sed 's/.*>//'; }
wait_job() { # <job path> -> final state
	local s
	for _ in $(seq 1 120); do
		s="$(job_state "$1")"
		case "$s" in succeeded | failed | cancelled) echo "$s"; return ;; esac
		sleep 2
	done
	echo timeout
}
job_error() { c "$B$1" | grep -o 'class="error" role="alert">[^<]*' | sed 's/.*>//' || true; }

DESKTOP=0
docker info --format '{{.OperatingSystem}}' | grep -q 'Docker Desktop' && DESKTOP=1

step "build"
mkdir -p "$WORK/img" bin
CGO_ENABLED=0 go build -o "$WORK/img/worker" ./cmd/worker
CGO_ENABLED=0 go build -o "$WORK/img/manager" ./cmd/manager
docker build --quiet -f scripts/e2e.Dockerfile -t "$IMAGE" "$WORK/img" >/dev/null
go build -o bin/manager ./cmd/manager
pass "worker image $IMAGE, bin/manager"

step "set up stack in $WORK"
mkdir -p "$DIR/data" "$ROOT/_repo" "$WORK/data"
echo v1 >"$DIR/data/value.txt"
if [[ "$DESKTOP" == 1 ]]; then
	printf 'services:\n  app:\n    image: busybox:1.36\n    command: ["sleep", "3600"]\n' >"$DIR/compose.yml"
	echo "  (Docker Desktop detected: test stack without bind mount)"
else
	cat >"$DIR/compose.yml" <<'YML'
services:
  app:
    image: busybox:1.36
    command: ["sleep", "3600"]
    volumes: ["./data:/data:ro"]
YML
fi
(cd "$DIR" && docker compose up --detach --quiet-pull >/dev/null 2>&1)
[[ "$(state app)" == running ]] || fail "app not running"
pass "stack $STACK running"

step "start manager"
STACKVAULT_HOSTNAME=e2e-host STACKS_ROOT="$ROOT" STACKVAULT_DATA_DIR="$WORK/data" \
	STACKVAULT_SECRET_KEY="$(head -c32 /dev/urandom | base64)" STACKVAULT_ALLOW_INSECURE_HTTP=true \
	STACKVAULT_LISTEN="127.0.0.1:$PORT" STACKVAULT_WORKER_IMAGE="$IMAGE" \
	./bin/manager >"$WORK/manager.log" 2>&1 &
MANAGER_PID=$!
for _ in $(seq 1 50); do curl -sf "$B/healthz" >/dev/null && break; sleep 0.2; done
curl -sf "$B/healthz" >/dev/null || fail "manager not healthy"
PW="$(grep -o 'INITIAL ADMIN PASSWORD (user "admin"): [^ ]*' "$WORK/manager.log" | awk '{print $NF}')"
[[ -n "$PW" ]] || fail "no initial admin password in the log"
c -o /dev/null -d "username=admin" --data-urlencode "password=$PW" "$B/login"
T="$(c "$B/account/password" | grep -o 'csrf_token" value="[^"]*' | head -1 | cut -d'"' -f3)"
c -o /dev/null --data-urlencode "csrf_token=$T" --data-urlencode "current=$PW" \
	-d "new=e2e+long+password&confirm=e2e+long+password" "$B/account/password"
c "$B/" | grep -q "href=\"/stacks/$STACK\"" || fail "stack not listed"
pass "logged in, stack discovered"

step "repository: save, initialize"
REPO_PW="e2e-repo-$RANDOM-secret"
c --data-urlencode "csrf_token=$T" --data-urlencode "location=$REPO" --data-urlencode "password=$REPO_PW" \
	"$B/stacks/$STACK/repository" | grep -q 'Initialize repository</button>' || fail "init not offered"
pass "no repository yet, init offered"
c --data-urlencode "csrf_token=$T" "$B/stacks/$STACK/repository/init" | grep -q 'Repository initialized. restic found 0 snapshots.' ||
	fail "init failed"
pass "repository initialized"

step "backup with pre-stop snapshot, a second one queued behind it"
submit_backup() { c -o /dev/null -w '%{http_code} %{redirect_url}' --data-urlencode "csrf_token=$T" "$@" "$B/stacks/$STACK/backup"; }
read -r CODE JOB <<<"$(submit_backup -d pre_stop=on)"
JOB="${JOB#"$B"}"
[[ "$CODE" == 303 && "$JOB" == /jobs/* ]] || fail "first backup: HTTP $CODE"
read -r CODE JOB2 <<<"$(submit_backup)"
JOB2="${JOB2#"$B"}"
[[ "$CODE" == 303 && "$JOB2" == /jobs/* ]] || fail "second backup: HTTP $CODE"
read -r CODE _ <<<"$(submit_backup)"
[[ "$CODE" == 409 ]] || fail "third backup while one is queued: HTTP $CODE, want 409"
pass "second backup queued, third refused"
# Follow the first backup through its live stream (SSE) until "done".
c -N --max-time 180 "$B$JOB/events?offset=0" >"$WORK/events.txt" || true
grep -q '^event: done$' "$WORK/events.txt" || fail "live stream did not end with done"
LINES="$(grep -c '^event: line$' "$WORK/events.txt")"
[[ "$LINES" -gt 5 ]] || fail "live stream sent only $LINES log lines"
grep -q 'final snapshot done' "$WORK/events.txt" || fail "live stream lacks the final snapshot line"
pass "live stream: $LINES log lines, details, done"
S="$(wait_job "$JOB")"
[[ "$S" == succeeded ]] || fail "backup $S: $(job_error "$JOB")"
pass "first backup succeeded ($JOB)"
c "$B$JOB" | grep -q 'Pre-stop snapshot' || fail "no pre-stop snapshot recorded"
pass "pre-stop and final snapshot recorded"
S="$(wait_job "$JOB2")"
[[ "$S" == succeeded ]] || fail "second backup $S: $(job_error "$JOB2")"
pass "second backup succeeded after the first ($JOB2)"
[[ "$(state app)" == running ]] || fail "app not restarted after backup"
pass "app restarted"

step "scheduled backup (every minute, waits up to 90 s)"
c -o /dev/null --data-urlencode "csrf_token=$T" --data-urlencode "cron_expr=* * * * *" "$B/stacks/$STACK/schedules"
SETTINGS="$(c "$B/stacks/$STACK/settings")"
SCHED="$(grep -o "/stacks/$STACK/schedules/[0-9]*/delete" <<<"$SETTINGS" | head -1 | cut -d/ -f5)"
[[ -n "$SCHED" ]] || fail "schedule not listed"
grep -q '<code>\* \* \* \* \*</code>' <<<"$SETTINGS" || fail "schedule expression not shown"
pass "schedule $SCHED added"
# scheduled_job: path of the newest job started by a schedule ("" if none).
scheduled_job() {
	c "$B/stacks/$STACK" | tr -d '\n' | sed 's#</tr>#</tr>\n#g' | grep 'muted">schedule<' |
		grep -o 'href="/jobs/[^"]*' | head -1 | cut -d'"' -f2 || true
}
JOB=""
for _ in $(seq 1 30); do
	JOB="$(scheduled_job)"
	[[ -n "$JOB" ]] && break
	sleep 3
done
[[ -n "$JOB" ]] || fail "no scheduled job within 90 s"
c -o /dev/null --data-urlencode "csrf_token=$T" "$B/stacks/$STACK/schedules/$SCHED/delete"
c "$B/stacks/$STACK/settings" | grep -q 'No schedules' || fail "schedule not deleted"
pass "scheduled job queued ($JOB), schedule deleted"
S="$(wait_job "$JOB")"
[[ "$S" == succeeded ]] || fail "scheduled backup $S: $(job_error "$JOB")"
c "$B$JOB" | grep -q '<td>schedule</td>' || fail "job page does not show the schedule trigger"
[[ "$(state app)" == running ]] || fail "app not restarted after the scheduled backup"
pass "scheduled backup succeeded, app restarted"

step "change data, then restore"
# Test only: the root worker wrote the local repository; let the non-root
# manager read it, then refresh the snapshot list through the UI.
docker run --rm -v "$ROOT/_repo:/r" busybox:1.36 chmod -R a+rX /r
sleep 10 # refresh cooldown
c -o /dev/null --data-urlencode "csrf_token=$T" "$B/stacks/$STACK/snapshots/refresh"
SNAP="$(c "$B/stacks/$STACK" | grep -o 'restore?snapshot=[0-9a-f]*' | head -1 | cut -d= -f2 || true)"
[[ -n "$SNAP" ]] || fail "no snapshot in the list"
pass "newest snapshot ${SNAP:0:8}"
echo v2 >"$DIR/data/value.txt"
JOB="$(c -o /dev/null -w '%{redirect_url}' --data-urlencode "csrf_token=$T" -d "snapshot_id=$SNAP" \
	-d safety_snapshot=on -d start=on "$B/stacks/$STACK/restore")"
JOB="${JOB#"$B"}"
S="$(wait_job "$JOB")"
[[ "$S" == succeeded ]] || fail "restore $S: $(job_error "$JOB")"
pass "restore succeeded ($JOB)"
[[ "$(cat "$DIR/data/value.txt")" == v1 ]] || fail "data not restored"
pass "data restored (v1)"
compgen -G "$ROOT/$STACK.old-*" >/dev/null || fail "previous directory not kept"
pass "previous directory kept"
[[ "$(state app)" == running ]] || fail "app not running after restore"
if [[ "$DESKTOP" == 0 ]]; then
	SEEN="$(docker compose --project-directory "$DIR" exec -T app cat /data/value.txt)"
	[[ "$SEEN" == v1 ]] || fail "app sees '$SEEN' instead of restored data"
	pass "app running and its bind mount shows restored data"
else
	pass "app running"
fi
c "$B$JOB" | grep -q 'Safety snapshot' || fail "no safety snapshot recorded"
pass "safety snapshot recorded"

step "restore with pinned image versions"
DIGEST="$(awk -F'\t' '$1 == "app" { print $4 }' "$DIR/metadata.txt")"
[[ "$DIGEST" == busybox@sha256:* ]] || fail "no repo digest for app in metadata.txt: '$DIGEST'"
JOB="$(c -o /dev/null -w '%{redirect_url}' --data-urlencode "csrf_token=$T" -d "snapshot_id=$SNAP" \
	-d safety_snapshot=on -d start=on -d images=pinned "$B/stacks/$STACK/restore")"
JOB="${JOB#"$B"}"
S="$(wait_job "$JOB")"
[[ "$S" == succeeded ]] || fail "pinned restore $S: $(job_error "$JOB")"
c "$B$JOB" | grep -q 'image versions pinned' || fail "job page does not show pinning"
c "$B$JOB" | grep -q 'image pinned' || fail "job log lacks the pinned image"
pass "pinned restore succeeded ($JOB)"
CID="$(docker compose --project-directory "$DIR" ps -q app)"
RUNNING="$(docker inspect --format '{{.Config.Image}}' "$CID")"
[[ "$RUNNING" == "$DIGEST" ]] || fail "app runs '$RUNNING', want '$DIGEST'"
grep -q 'image: busybox:1.36' "$DIR/compose.yml" || fail "compose.yml was changed"
pass "app runs $DIGEST, compose.yml unchanged"

step "cleanup and secrets"
sleep 3
LEFT=0
for job in "$WORK"/data/jobs/*/; do
	docker container inspect "stackvault-job-$(basename "$job" | tr 'A-Z' 'a-z')" >/dev/null 2>&1 && LEFT=$((LEFT + 1))
done
[[ "$LEFT" == 0 ]] || fail "$LEFT worker container(s) left"
pass "worker containers removed"
grep -q -- "$REPO_PW" "$WORK/manager.log" && fail "repository password in the manager log"
docker run --rm -v "$WORK/data:/d" busybox:1.36 sh -c "cat /d/jobs/*/*" | grep -q -- "$REPO_PW" &&
	fail "repository password in a job file"
pass "repository password in no log or job file"

printf '\n\033[32mall end-to-end checks passed\033[0m\n'
