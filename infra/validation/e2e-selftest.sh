#!/usr/bin/env bash
# Harness self-test for scripts/infra-e2e-env. It proves the scenario contract
# (environment, control, db-read, media-ls, admin, faults) against the real
# stack. It is NOT issue #21 acceptance: it uploads nothing and asserts no
# application behaviour beyond readiness.
set -euo pipefail

ctl=${NMCP_E2E_CONTROL:?run through scripts/infra-e2e-env run}
step() { printf '=== selftest: %s\n' "$*"; }
fail() { printf -- '--- FAIL: selftest: %s\n' "$*"; exit 1; }

for name in NMCP_E2E_API_URL NMCP_E2E_FILES_URL NMCP_E2E_FILES_CA_FILE NMCP_E2E_ARTIFACT_DIR NMCP_E2E_DEADLINE NMCP_E2E_HEAD_SHA; do
  [ -n "${!name:-}" ] || fail "$name is not set"
done
[ "$NMCP_E2E_DEADLINE" -gt "$(date +%s)" ] || fail "deadline already passed"
[ -w "$NMCP_E2E_ARTIFACT_DIR" ] || fail "artifact dir not writable"

step "API readiness and Nginx TLS with the exported CA"
curl --fail --silent --show-error "$NMCP_E2E_API_URL/readyz" >/dev/null || fail "api readyz"
curl --fail --silent --show-error --cacert "$NMCP_E2E_FILES_CA_FILE" \
  "${NMCP_E2E_FILES_URL%/files}/files-healthz" >/dev/null || fail "files healthz"

step "db-read is a read-only session"
[ "$(printf 'SELECT 1;\n' | "$ctl" db-read)" = 1 ] || fail "db-read SELECT"
if printf 'CREATE TABLE e2e_selftest_write (x int);\n' | "$ctl" db-read 2>/dev/null; then
  fail "db-read accepted a write"
fi

step "media-ls lists the real tree (storage-init layout, no fixtures)"
"$ctl" media-ls >"$NMCP_E2E_ARTIFACT_DIR/selftest-media-ls.jsonl"
python3 - "$NMCP_E2E_ARTIFACT_DIR/selftest-media-ls.jsonl" <<'PY' || fail "media-ls shape"
import json, sys
entries = [json.loads(line) for line in open(sys.argv[1], encoding="utf-8")]
paths = {e["path"] for e in entries}
for required in ("originals", "renditions", "tmp", ".quarantine", "backups"):
    assert required in paths, required
assert all(e["type"] == "dir" for e in entries), entries
PY

step "admin runs nmcp-admin as nmcp_migrator"
"$ctl" admin -- migrate status --json >"$NMCP_E2E_ARTIFACT_DIR/selftest-admin.json" ||
  fail "nmcp-admin migrate status"

step "control kill worker SIGKILL prints the pid; start gives a new process"
old=$("$ctl" control kill worker KILL) || fail "kill worker"
old_pid=${old#pid=}; old_pid=${old_pid%% *}
"$ctl" control start worker || fail "start worker"
new_pid=$("$ctl" control pid worker)
[ "$new_pid" -gt 0 ] && [ "$new_pid" != "$old_pid" ] || fail "worker pid not replaced ($old_pid -> $new_pid)"

step "SIGSTOP/SIGCONT on worker"
"$ctl" control kill worker STOP >/dev/null || fail "stop signal"
"$ctl" control kill worker CONT >/dev/null || fail "cont signal"

step "DB loss: stop postgres, API not ready, start, API ready"
"$ctl" control stop postgres || fail "stop postgres"
if curl --fail --silent --max-time 5 "$NMCP_E2E_API_URL/readyz" >/dev/null; then
  fail "api reported ready without postgres"
fi
"$ctl" control start postgres || fail "start postgres"
# Whether api/worker survive DB loss is application behaviour asserted by the
# #21 scenario, not by this self-test; restart whichever exited.
for service in api worker; do
  if [ "$("$ctl" control pid "$service")" = 0 ]; then "$ctl" control start "$service" || fail "start $service"; fi
done
deadline=$((SECONDS + 60))
until curl --fail --silent --max-time 5 "$NMCP_E2E_API_URL/readyz" >/dev/null; do
  [ "$SECONDS" -lt "$deadline" ] || fail "api not ready after postgres restart"
  sleep 1
done

step "db-read while api and worker are stopped"
"$ctl" control stop api || fail "stop api"
"$ctl" control stop worker || fail "stop worker"
[ "$(printf 'SELECT 1;\n' | "$ctl" db-read)" = 1 ] || fail "db-read with api/worker stopped"
"$ctl" control start api || fail "start api"
"$ctl" control start worker || fail "start worker"

probe() {
  # Run a write probe as the service user inside the API container; prints errno text on failure.
  local id
  id=$(docker container ls --quiet \
    --filter "label=com.docker.compose.project=$NMCP_E2E_PROJECT" \
    --filter label=com.docker.compose.service=api)
  [ -n "$id" ] || fail "api container not found"
  docker container exec "$id" sh -c "$1" 2>&1
}

step "fault media-readonly: whole media filesystem rejects writes with EROFS"
"$ctl" fault media-readonly on || fail "readonly on"
out=$(probe 'touch /var/lib/nmcp/media/tmp/.e2e-probe' || true)
case "$out" in *"Read-only file system"*) ;; *) fail "expected EROFS, got: $out" ;; esac
"$ctl" fault media-readonly off || fail "readonly off"
probe 'touch /var/lib/nmcp/media/tmp/.e2e-probe && rm /var/lib/nmcp/media/tmp/.e2e-probe' >/dev/null ||
  fail "write after readonly off"

step "fault media-full: shrink to used size gives ENOSPC; off restores"
"$ctl" fault media-full on 0 || fail "full on"
out=$(probe 'head -c 1048576 /dev/zero >/var/lib/nmcp/media/originals/.e2e-probe; status=$?; rm -f /var/lib/nmcp/media/originals/.e2e-probe; exit $status' || true)
case "$out" in *"No space left on device"*) ;; *) fail "expected ENOSPC, got: $out" ;; esac
"$ctl" fault media-full off || fail "full off"
probe 'head -c 1048576 /dev/zero >/var/lib/nmcp/media/originals/.e2e-probe && rm /var/lib/nmcp/media/originals/.e2e-probe' >/dev/null ||
  fail "write after full off"

step "media tree is unchanged by the faults"
"$ctl" media-ls >"$NMCP_E2E_ARTIFACT_DIR/selftest-media-ls-after.jsonl"
cmp -s "$NMCP_E2E_ARTIFACT_DIR/selftest-media-ls.jsonl" "$NMCP_E2E_ARTIFACT_DIR/selftest-media-ls-after.jsonl" ||
  fail "media tree changed"

echo "--- PASS: selftest (harness contract only; NOT #21 acceptance)"
echo PASS
