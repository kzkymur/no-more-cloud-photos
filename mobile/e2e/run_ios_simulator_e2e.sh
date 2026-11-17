#!/usr/bin/env bash
# iOS simulator end-to-end test of the uploader against stub_core_api.py.
#
#   run_ios_simulator_e2e.sh <path/to/App.app> <bundle id> <work dir>
#
# 1. Generates JPEG + HEIC fixtures with GPS EXIF (exiftool), plus a byte-identical
#    copy of one JPEG as a second library asset.
# 2. Boots a fresh simulator, imports the fixtures into Photos, grants full
#    photo access, installs the Release build and launches it pointed at the stub
#    (server URL via the NSArgumentDomain, so no UI typing is needed).
# 3. Phase 1: the stub drops the response of the first new upload. Every fixture
#    must still arrive byte-identical (sha256) with GPS intact.
# 4. Phase 2: relaunch. The dropped item is retried with the same key, so the
#    server replays it (Idempotency-Replayed). The duplicate copy settles as
#    409 duplicate_media.
# 5. Phase 3: relaunch again. The ledger must not send anything again.
set -euo pipefail

APP=$1
BUNDLE=$2
WORK=$3
PORT=8787
HERE=$(cd "$(dirname "$0")" && pwd)
mkdir -p "$WORK/fixtures" "$WORK/store"
LOG="$WORK/stub.jsonl"
: > "$LOG"

log() { printf '\n== %s\n' "$*"; }
diagnose() {
  [ -n "${UDID:-}" ] || return 0
  echo "-- diagnostics"
  xcrun simctl io "$UDID" screenshot "$WORK/failure.png" >/dev/null 2>&1 || true
  xcrun simctl spawn "$UDID" log show --last 20m --style compact \
    --predicate 'subsystem == "io.nmcp.uploader" OR process == "NMCPPhotos"' > "$WORK/app.log" 2>&1 || true
  tail -80 "$WORK/app.log" || true
  for f in "$WORK"/app-stdout.log "$WORK"/app-stderr.log; do [ -f "$f" ] && { echo "-- $f"; tail -60 "$f"; }; done
  ls -t ~/Library/Logs/DiagnosticReports/NMCPPhotos* 2>/dev/null | head -1 | xargs -I{} sh -c 'echo "-- crash {}"; head -80 "{}"' || true
  xcrun simctl spawn "$UDID" launchctl list 2>/dev/null | grep -i nmcp || echo "(app process not running)"
  curl -s -m 3 "http://localhost:$PORT/readyz" || echo "(stub not reachable from host)"
}
fail() { echo "E2E FAIL: $*"; diagnose; exit 1; }

log "fixtures"
python3 -I - "$WORK/fixtures" <<'PY'
import struct, sys, zlib
out = sys.argv[1]
def png(path, rgb, w=64, h=48):
    raw = b"".join(b"\x00" + bytes(rgb) * w for _ in range(h))
    def chunk(t, d): return struct.pack(">I", len(d)) + t + d + struct.pack(">I", zlib.crc32(t + d) & 0xffffffff)
    data = b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack(">IIBBBBB", w, h, 8, 2, 0, 0, 0)) \
        + chunk(b"IDAT", zlib.compress(raw)) + chunk(b"IEND", b"")
    open(path, "wb").write(data)
png(f"{out}/a.png", (200, 30, 30)); png(f"{out}/b.png", (30, 200, 30)); png(f"{out}/c.png", (30, 30, 200))
PY
F=$WORK/fixtures
sips -s format jpeg "$F/a.png" --out "$F/nmcp-e2e-a.jpg" >/dev/null
sips -s format jpeg "$F/b.png" --out "$F/nmcp-e2e-b.jpg" >/dev/null
sips -s format heic "$F/c.png" --out "$F/nmcp-e2e-c.heic" >/dev/null
rm -f "$F"/*.png
exiftool -q -overwrite_original -GPSLatitude=35.658581 -GPSLatitudeRef=N -GPSLongitude=139.745433 \
  -GPSLongitudeRef=E -DateTimeOriginal="2026:11:16 10:15:00" "$F"/nmcp-e2e-*
cp "$F/nmcp-e2e-a.jpg" "$F/nmcp-e2e-a-copy.jpg"
for f in "$F"/*; do
  exiftool -s3 -GPSLatitude "$f" | grep -q . || fail "fixture $f has no GPS"
done
(cd "$F" && shasum -a 256 nmcp-e2e-a.jpg nmcp-e2e-b.jpg nmcp-e2e-c.heic) | tee "$WORK/fixtures.sha256"
EXPECTED=$(cut -d' ' -f1 "$WORK/fixtures.sha256" | sort)

log "stub Core API on :$PORT (drop first response)"
python3 -I "$HERE/stub_core_api.py" --port "$PORT" --store "$WORK/store" --log "$LOG" --drop-first 1 &
STUB=$!
trap 'kill $STUB 2>/dev/null || true; [ -n "${UDID:-}" ] && xcrun simctl shutdown "$UDID" >/dev/null 2>&1 || true' EXIT
for _ in $(seq 50); do curl -fs "http://localhost:$PORT/readyz" >/dev/null && break; sleep 0.2; done

log "simulator"
RUNTIME=$(xcrun simctl list runtimes -j | python3 -I -c 'import json,sys; r=[x for x in json.load(sys.stdin)["runtimes"] if x["platform"]=="iOS" and x["isAvailable"]]; print(sorted(r,key=lambda x:[int(p) for p in x["version"].split(".")])[-1]["identifier"])')
# A device type the runtime supports: take it from the runner's preinstalled devices.
DEVICE=$(xcrun simctl list devices available -j | python3 -I -c 'import json,sys; d=json.load(sys.stdin)["devices"].get(sys.argv[1],[]); i=[x["deviceTypeIdentifier"] for x in d if "iPhone" in x["name"] and "SE" not in x["name"]]; print(i[0] if i else "")' "$RUNTIME")
[ -n "$DEVICE" ] || DEVICE=com.apple.CoreSimulator.SimDeviceType.iPhone-16
UDID=$(xcrun simctl create nmcp-e2e "$DEVICE" "$RUNTIME")
echo "runtime=$RUNTIME device=$DEVICE udid=$UDID"
xcrun simctl boot "$UDID"
xcrun simctl bootstatus "$UDID" -b >/dev/null
xcrun simctl addmedia "$UDID" "$F"/nmcp-e2e-*
xcrun simctl install "$UDID" "$APP"
xcrun simctl privacy "$UDID" grant photos "$BUNDLE"

launch() {
  xcrun simctl terminate "$UDID" "$BUNDLE" >/dev/null 2>&1 || true
  xcrun simctl launch --stdout="$WORK/app-stdout.log" --stderr="$WORK/app-stderr.log" "$UDID" "$BUNDLE" \
    -nmcp.uploader.serverUrl "http://localhost:$PORT" \
    -nmcp.uploader.wifiOnly '<false/>' \
    -nmcp.uploader.autoUpload '<true/>' >/dev/null
}
uploads() { python3 -I -c 'import json,sys; [print(l.strip()) for l in open(sys.argv[1]) if json.loads(l).get("event")=="upload"]' "$LOG"; }
stored_fixture_shas() { python3 -I - "$LOG" <<'PY'
import json, sys
s = {e["sha256"] for e in map(json.loads, open(sys.argv[1])) if e.get("event") == "upload" and e["status"] == 201}
print("\n".join(sorted(s)))
PY
}
wait_for() { # seconds, description, condition...
  local deadline=$(( $(date +%s) + $1 )) what=$2; shift 2
  until "$@"; do
    [ "$(date +%s)" -lt "$deadline" ] || { uploads; fail "timeout waiting for $what"; }
    sleep 3
  done
}
all_fixtures_stored() { [ "$(comm -23 <(echo "$EXPECTED") <(stored_fixture_shas) | wc -l | tr -d ' ')" = 0 ]; }
dropped_was_replayed() { python3 -I - "$LOG" <<'PY'
import json, sys
ev = [e for e in map(json.loads, open(sys.argv[1])) if e.get("event") == "upload"]
dropped = {e["key"] for e in ev if e["dropped"]}
replayed = {e["key"] for e in ev if e["replayed"]}
sys.exit(0 if dropped and dropped <= replayed else 1)
PY
}
duplicate_settled() { grep -q '"code": "duplicate_media"' "$LOG"; }

log "phase 1: upload (first response is lost)"
launch
wait_for 420 "all fixtures stored" all_fixtures_stored
uploads

log "phase 2: relaunch -> retry of the lost response is replayed"
launch
wait_for 300 "replay of the dropped upload" dropped_was_replayed
wait_for 120 "duplicate copy settled as duplicate_media" duplicate_settled
sleep 10
uploads

log "phase 3: relaunch -> nothing is sent again"
BEFORE=$(uploads | wc -l | tr -d ' ')
launch
sleep 45
AFTER=$(uploads | wc -l | tr -d ' ')
[ "$AFTER" = "$BEFORE" ] || { uploads | tail -n +$((BEFORE + 1)); fail "ledger re-sent $((AFTER - BEFORE)) item(s)"; }

log "originals are byte-identical and keep GPS"
for sha in $EXPECTED; do
  [ -f "$WORK/store/$sha" ] || fail "missing stored original $sha"
  [ "$(shasum -a 256 "$WORK/store/$sha" | cut -d' ' -f1)" = "$sha" ] || fail "stored bytes differ for $sha"
  exiftool -s3 -GPSLatitude "$WORK/store/$sha" | grep -q . || fail "GPS missing in uploaded $sha"
done
python3 -I - "$LOG" <<'PY'
import json, re, sys
for e in map(json.loads, open(sys.argv[1])):
    if e.get("event") == "rejected":
        sys.exit("stub rejected a request: %s" % e)
    if e.get("event") == "upload" and not re.fullmatch(r"nmcp1-[0-9a-f]{64}", e["key"]):
        sys.exit("unexpected key format: %s" % e["key"])
PY
xcrun simctl io "$UDID" screenshot "$WORK/dashboard.png" >/dev/null 2>&1 || true
echo "E2E PASS: $(uploads | wc -l | tr -d ' ') POST /media requests, $(echo "$EXPECTED" | wc -l | tr -d ' ') fixtures byte-identical with GPS, lost response replayed, duplicate settled, no re-send"
