#!/usr/bin/env bash
set -euo pipefail
script_dir=$(cd -- "$(dirname -- "$0")" && pwd)
source "$script_dir/build-pinned-toolchain.sh"
# Keep fixtures within the workspace, never invoke a build or real network.
fixture=$(mktemp -d "$script_dir/.acquisition-test.XXXXXX")
[[ "$fixture" == "$script_dir"/.acquisition-test.* ]] || exit 1
trap 'rm -rf -- "$fixture"' EXIT
mkdir -p "$fixture/bin" "$fixture/work/downloads"
export FIXTURE_ROOT=$fixture
export PATH="$fixture/bin:$PATH"
work="$fixture/work"
printf 'the pinned fixture archive\n' > "$fixture/good"
printf 'different bytes\n' > "$fixture/bad"
digest=$(sha256sum "$fixture/good" | cut -d ' ' -f 1)
cat > "$fixture/bin/curl" <<'CURL'
#!/usr/bin/env bash
set -euo pipefail
# Check that production acquisition always supplies both HTTPS restrictions
# and hard connect/overall limits; no curl retry flag is allowed.
[[ "$*" == *"--proto =https --proto-redir =https --connect-timeout 10 --max-time 120"* ]]
[[ "$*" != *--retry* ]]
url=${@: -3:1}
[[ ${@: -2:1} == --output ]]
out=${@: -1}
printf '%s\n' "$url" >> "$FIXTURE_ROOT/calls"
case "$url" in
  https://primary/archive) mode=$PRIMARY_MODE ;;
  https://mirror/archive) mode=$MIRROR_MODE ;;
  *) exit 2 ;;
esac
case "$mode" in
  good|bad) cat "$FIXTURE_ROOT/$mode" > "$out" ;;
  fail) printf 'partial transport bytes' > "$out"; exit 28 ;;
  *) exit 2 ;;
esac
CURL
chmod +x "$fixture/bin/curl"
run_case() {
  local label=$1 primary=$2 mirror=$3 expected=$4 calls=$5 cache=${6:-none}
  work="$fixture/work/$label"
  mkdir -p "$work/downloads"
  : > "$fixture/calls"
  export PRIMARY_MODE=$primary MIRROR_MODE=$mirror
  if [[ "$cache" != none ]]; then
    cp "$fixture/$cache" "$work/downloads/archive"
  fi
  if fetch_verified_source archive "$digest" https://primary/archive https://mirror/archive; then
    [[ "$expected" == success ]]
    cmp "$fixture/good" "$work/downloads/archive"
  else
    [[ "$expected" == failure ]]
    if [[ "$cache" == none ]]; then [[ ! -e "$work/downloads/archive" ]]; fi
  fi
  [[ $(wc -l < "$fixture/calls") -eq "$calls" ]]
  [[ -z $(find "$work/downloads" -name '*.partial.*' -print) ]]
  if [[ "$calls" -eq 2 ]]; then
    [[ $(sed -n '1p' "$fixture/calls") == https://primary/archive ]]
    [[ $(sed -n '2p' "$fixture/calls") == https://mirror/archive ]]
  fi
  printf 'PASS %s\n' "$label"
}
run_case primary-good good bad success 1
run_case primary-unreachable fail good success 2
run_case primary-wrong-hash bad good failure 1
run_case mirror-wrong-hash fail bad failure 2
run_case bounded-both-unreachable fail fail failure 2
run_case verified-cache fail fail success 0 good
run_case corrupt-cache good good failure 0 bad
# Identity remains pinned at the production call site and in its manifest.
python3 - "$script_dir/build-pinned-toolchain.sh" <<'PY'
import pathlib, sys
s = pathlib.Path(sys.argv[1]).read_text()
assert '''fetch_verified_source ffmpeg-9.0.2.tar.xz \\
  8c3850283eb25fa026482078a04051e0be17347b09ef81a0849bec15a96e002e \\
  https://ffmpeg.org/releases/ffmpeg-9.0.2.tar.xz \\
  https://distfiles.macports.org/ffmpeg/ffmpeg-9.0.2.tar.xz''' in s
assert "'ffmpeg_version=9.0.2'" in s
assert "'ffmpeg_source_sha256=8c3850283eb25fa026482078a04051e0be17347b09ef81a0849bec15a96e002e'" in s
PY
printf 'PASS pinned-source-identity\n'
