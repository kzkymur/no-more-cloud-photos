#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "$0")" && pwd)
root=$(mktemp -d)
trap 'rm -rf "$root"' EXIT
prefix="$root/prefix"
mkdir -p "$root/bin" "$prefix/lib/pkgconfig" "$prefix/share/nmcp"
cat >"$root/bin/pkg-config" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
case "$1 $2" in
  '--modversion aom') printf '3.8.2\n' ;;
  '--variable=pcfiledir aom') printf '%s\n' "${FAKE_PC_DIR:-$FAKE_PREFIX/lib/pkgconfig}" ;;
  '--variable=libdir aom') printf '%s\n' "$FAKE_PREFIX/lib" ;;
  *) exit 2 ;;
esac
EOF
chmod +x "$root/bin/pkg-config"
export FAKE_PREFIX="$prefix"
export PATH="$root/bin:$PATH"

printf 'int aom_codec_version(void) { return 30802; }\n' >"$root/aom.c"
cc -shared -fPIC -Wl,-soname,libaom.so.3 -o "$prefix/lib/libaom.so.3.8.2" "$root/aom.c"
digest=$(sha256sum "$prefix/lib/libaom.so.3.8.2")
digest=${digest%% *}
printf '%s\n' \
  'prefix='"$prefix" \
  'libdir=${prefix}/lib' \
  'Name: aom' \
  'Description: Alliance for Open Media AV1 codec library v3.8.2.' \
  'Version: 3.8.2' \
  'Libs: -L${libdir} -laom' \
  >"$prefix/lib/pkgconfig/aom.pc"
printf '%s\n' \
  'manifest_version=1' \
  'libaom_version=3.8.2' \
  'libaom_commit=615b5f541e4434aebd993036bc97ebc1a77ebc25' \
  'libaom_source_sha256=eb0bfa625cd17849be2e17ffd38bf8e1dc67b7c7787e7152250a4075d05f245f' \
  'libaom_object=lib/libaom.so.3.8.2' \
  "libaom_object_sha256=$digest" \
  >"$prefix/share/nmcp/still-toolchain.manifest"

"$script_dir/validate-inherited-toolchain.sh" "$prefix" >/dev/null

if FAKE_PC_DIR=/usr/lib/pkgconfig "$script_dir/validate-inherited-toolchain.sh" "$prefix" >/dev/null 2>&1; then
  printf 'accepted substituted aom.pc\n' >&2
  exit 1
fi

printf 'int aom_codec_version(void) { return 30802; } int substituted(void) { return 1; }\n' >"$root/aom.c"
cc -shared -fPIC -Wl,-soname,libaom.so.3 -o "$prefix/lib/libaom.so.3.8.2" "$root/aom.c"
if "$script_dir/validate-inherited-toolchain.sh" "$prefix" >/dev/null 2>&1; then
  printf 'accepted substituted libaom object\n' >&2
  exit 1
fi
