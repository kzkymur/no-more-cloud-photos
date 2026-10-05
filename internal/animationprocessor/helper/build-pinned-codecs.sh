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

download libwebp-1.6.0.tar.gz \
  https://storage.googleapis.com/downloads.webmproject.org/releases/webp/libwebp-1.6.0.tar.gz \
  e4ab7009bf0629fd11982d4c2aa83964cf244cffba7347ecd39019a9e38c4564
download giflib-5.2.2.tar.gz \
  https://sourceforge.net/projects/giflib/files/giflib-5.2.2.tar.gz/download \
  be7ffbd057cadebe2aa144542fd90c6838c6a083b5e8a9048b8ee3b66b29d5fb

rm -rf "$work/source/libwebp-1.6.0" "$work/source/giflib-5.2.2" \
  "$work/build/libwebp"
tar -xzf "$work/downloads/libwebp-1.6.0.tar.gz" -C "$work/source"
tar -xzf "$work/downloads/giflib-5.2.2.tar.gz" -C "$work/source"

cmake -S "$work/source/libwebp-1.6.0" -B "$work/build/libwebp" -G Ninja \
  -DCMAKE_BUILD_TYPE=Release -DCMAKE_INSTALL_PREFIX="$prefix" \
  -DBUILD_SHARED_LIBS=ON -DWEBP_BUILD_ANIM_UTILS=OFF \
  -DWEBP_BUILD_CWEBP=OFF -DWEBP_BUILD_DWEBP=OFF -DWEBP_BUILD_EXTRAS=OFF \
  -DWEBP_BUILD_GIF2WEBP=OFF -DWEBP_BUILD_IMG2WEBP=OFF \
  -DWEBP_BUILD_LIBWEBPMUX=ON -DWEBP_BUILD_VWEBP=OFF \
  -DWEBP_BUILD_WEBPINFO=OFF -DWEBP_BUILD_WEBPMUX=OFF
cmake --build "$work/build/libwebp" --parallel 2
cmake --install "$work/build/libwebp"

(
  cd "$work/source/giflib-5.2.2"
  make -j2 libgif.so
  install -Dm755 libgif.so.7.2.0 "$prefix/lib/libgif.so.7.2.0"
  ln -sfn libgif.so.7.2.0 "$prefix/lib/libgif.so.7"
  ln -sfn libgif.so.7 "$prefix/lib/libgif.so"
  install -Dm644 gif_lib.h "$prefix/include/gif_lib.h"
)

PKG_CONFIG_PATH="$prefix/lib/pkgconfig:$prefix/lib64/pkgconfig${PKG_CONFIG_PATH:+:$PKG_CONFIG_PATH}" \
  pkg-config --modversion libwebp libwebpdemux libwebpmux
