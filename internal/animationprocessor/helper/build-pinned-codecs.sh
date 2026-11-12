#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 2 || "$1" != /* || "$2" != /* ]]; then
  printf 'usage: %s ABSOLUTE_PREFIX ABSOLUTE_WORK\n' "$0" >&2
  exit 2
fi

prefix=$1
work=$2
mkdir -p "$prefix" "$work/downloads" "$work/source" "$work/build"

download() {
  local name=$1 url=$2 digest=$3
  local target="$work/downloads/$name"
  if [[ ! -f "$target" ]]; then
    curl --fail --location --silent --show-error "$url" --output "$target"
  fi
  printf '%s  %s\n' "$digest" "$target" | sha256sum --check
}

download giflib-5.2.2.tar.gz \
  https://sourceforge.net/projects/giflib/files/giflib-5.2.2.tar.gz/download \
  be7ffbd057cadebe2aa144542fd90c6838c6a083b5e8a9048b8ee3b66b29d5fb

rm -rf "$work/source/giflib-5.2.2"
tar -xzf "$work/downloads/giflib-5.2.2.tar.gz" -C "$work/source"

# libwebp 1.6.0 is INHERITED from the still toolchain (built before libvips so
# the whole prefix has exactly one webp). Validate it; never reinstall it.
manifest="$prefix/share/nmcp/still-webp.manifest"
[[ -f "$manifest" && ! -L "$manifest" ]] || { printf 'still toolchain webp manifest missing; build it first\n' >&2; exit 1; }
grep -qx 'manifest_version=1' "$manifest"
grep -qx 'libwebp_version=1.6.0' "$manifest"
grep -qx 'libwebp_source_sha256=e4ab7009bf0629fd11982d4c2aa83964cf244cffba7347ecd39019a9e38c4564' "$manifest"
[[ "$(PKG_CONFIG_PATH= PKG_CONFIG_LIBDIR="$prefix/lib/pkgconfig:$prefix/lib64/pkgconfig" \
  pkg-config --modversion libwebp libwebpdemux libwebpmux libsharpyuv | sort -u)" == 1.6.0 ]]
for lib in libwebp libwebpmux libwebpdemux libsharpyuv; do
  object=$(sed -n "s/^${lib}_object=//p" "$manifest")
  digest=$(sed -n "s/^${lib}_object_sha256=//p" "$manifest")
  [[ -n "$object" && -n "$digest" && -f "$prefix/$object" && ! -L "$prefix/$object" ]] ||
    { printf 'inherited %s object missing\n' "$lib" >&2; exit 1; }
  printf '%s  %s\n' "$digest" "$prefix/$object" | sha256sum --check --quiet
  [[ "$(readelf -d "$prefix/$object" | sed -n 's/.*(RUNPATH).*\[\(.*\)\]/\1/p')" == *"$prefix/lib"* ]] ||
    { printf 'inherited %s lacks RUNPATH %s\n' "$lib" "$prefix/lib" >&2; exit 1; }
done

(
  cd "$work/source/giflib-5.2.2"
  make -j2 libgif.so
  install -Dm755 libgif.so "$prefix/lib/libgif.so.7.2.0"
  ln -sfn libgif.so.7.2.0 "$prefix/lib/libgif.so.7"
  ln -sfn libgif.so.7 "$prefix/lib/libgif.so"
  install -Dm644 gif_lib.h "$prefix/include/gif_lib.h"
)

PKG_CONFIG_PATH= PKG_CONFIG_LIBDIR="$prefix/lib/pkgconfig:$prefix/lib64/pkgconfig" \
  pkg-config --modversion libwebp libwebpdemux libwebpmux
