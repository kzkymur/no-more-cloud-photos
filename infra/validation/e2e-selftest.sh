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

step "image-sha256: digests from a fresh one-shot of the worker/admin image"
digests=$("$ctl" image-sha256 /opt/nmcp/share/sRGB2014.icc /usr/bin/prlimit) || fail "image-sha256"
case "$digests" in
  "384b832de3412066743b52a75ee906b6fb9fb8d9e09e936fc2c43223815c6e0a  /opt/nmcp/share/sRGB2014.icc"*) ;;
  *) fail "image-sha256 ICC digest: $digests" ;;
esac
[ "$(printf '%s\n' "$digests" | wc -l)" = 2 ] || fail "image-sha256 line count: $digests"
if "$ctl" image-sha256 /etc/passwd 2>/dev/null; then fail "image-sha256 accepted a path outside the allowlist"; fi
if "$ctl" image-sha256 /opt/nmcp/../../etc/passwd 2>/dev/null; then fail "image-sha256 accepted a non-canonical path"; fi
if "$ctl" image-sha256 /opt/nmcp/bin/does-not-exist 2>/dev/null; then fail "image-sha256 accepted a missing file"; fi

step "admin runs nmcp-admin as nmcp_migrator"
"$ctl" admin -- migrate status --json >"$NMCP_E2E_ARTIFACT_DIR/selftest-admin.json" ||
  fail "nmcp-admin migrate status"

step "admin one-shot mounts: fixtures and media read-only, private /e2e/work outside media"
case "$NMCP_E2E_FIXTURES_DIR" in */no-fixtures) ;; *) fail "self-test expects the empty fixture dir, got $NMCP_E2E_FIXTURES_DIR" ;; esac
marker="selftest-$$"
printf '%s' "$marker" >"$NMCP_E2E_FIXTURES_DIR/selftest.marker"
chmod 0644 "$NMCP_E2E_FIXTURES_DIR/selftest.marker"
# shellcheck disable=SC2016 # expanded by the container shell
mounts=$(docker compose --project-name "$NMCP_E2E_PROJECT" \
  --file "$NMCP_E2E_ROOT/infra/validation/compose.yaml" --file "$NMCP_E2E_ROOT/infra/validation/compose.e2e.yaml" \
  run --rm --no-deps -T --entrypoint sh migrate -eu -c '
    cat /e2e/fixtures/selftest.marker; echo
    if touch /e2e/fixtures/x 2>/dev/null; then echo fixtures-writable; fi
    echo w >/e2e/work/probe && rm /e2e/work/probe && echo work-writable
    stat -c "%a %u" /e2e/work
    [ "$(stat -c %d /e2e/work)" != "$(stat -c %d /e2e/fixtures)" ] && echo work-separate
    [ "$NMCP_STORAGE_ROOT" = /var/lib/nmcp/media ] && echo storage-root
    [ -d /var/lib/nmcp/media/originals ] && echo media-visible
    if touch /var/lib/nmcp/media/tmp/.e2e-admin-probe 2>/dev/null; then echo media-writable; fi') || fail "admin mount probe"
rm -f "$NMCP_E2E_FIXTURES_DIR/selftest.marker"
# admin -- uses the same `compose run --no-deps -T migrate` invocation; prove
# that stdin reaches the one-shot process (certify reads its spec from -).
echoed=$(printf 'spec-%s' "$marker" | docker compose --project-name "$NMCP_E2E_PROJECT" \
  --file "$NMCP_E2E_ROOT/infra/validation/compose.yaml" --file "$NMCP_E2E_ROOT/infra/validation/compose.e2e.yaml" \
  run --rm --no-deps -T --entrypoint /usr/bin/timeout migrate 30s cat) || fail "stdin probe"
[ "$echoed" = "spec-$marker" ] || fail "stdin not forwarded to the admin one-shot: $echoed"
expected="$marker
work-writable
700 10001
work-separate
storage-root
media-visible"
[ "$mounts" = "$expected" ] || fail "admin mounts: $mounts"

step "release lane: set-failpoint is refused and the lane is reported"
[ "${NMCP_E2E_LANE:-}" = release ] && [ "${NMCP_E2E_FAILPOINTS:-}" = 0 ] || fail "lane env: ${NMCP_E2E_LANE:-unset}/${NMCP_E2E_FAILPOINTS:-unset}"
status=0; "$ctl" control set-failpoint worker none 2>/dev/null || status=$?
[ "$status" = 2 ] || fail "set-failpoint in the release lane returned $status, want 2"

step "control kill worker SIGKILL prints the pid; start gives a new process"
old=$("$ctl" control kill worker KILL) || fail "kill worker"
old_pid=${old#pid=}; old_pid=${old_pid%% *}
"$ctl" control start worker || fail "start worker"
new_pid=$("$ctl" control pid worker)
if [ "$new_pid" -le 0 ] || [ "$new_pid" = "$old_pid" ]; then
  fail "worker pid not replaced ($old_pid -> $new_pid)"
fi

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
# shellcheck disable=SC2016 # expanded by the container shell
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
