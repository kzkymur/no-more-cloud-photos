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

download zimg-3.0.6.tar.gz \
  https://github.com/sekrit-twc/zimg/archive/refs/tags/release-3.0.6.tar.gz \
  be89390f13a5c9b2388ce0f44a5e89364a20c1c57ce46d382b1fcc3967057577
download SVT-AV1-v4.2.0.tar.gz \
  https://gitlab.com/AOMediaCodec/SVT-AV1/-/archive/v4.2.0/SVT-AV1-v4.2.0.tar.gz \
  c7b13c4a84bd3751aa35fcc72be13e6875467e7c2216879251a486e5b1e4e740
download ffmpeg-9.0.2.tar.xz \
  https://ffmpeg.org/releases/ffmpeg-9.0.2.tar.xz \
  8c3850283eb25fa026482078a04051e0be17347b09ef81a0849bec15a96e002e

rm -rf "$work/source/zimg-release-3.0.6" "$work/source/SVT-AV1-v4.2.0" \
  "$work/source/ffmpeg-9.0.2" "$work/build/svt-av1"
tar -xzf "$work/downloads/zimg-3.0.6.tar.gz" -C "$work/source"
tar -xzf "$work/downloads/SVT-AV1-v4.2.0.tar.gz" -C "$work/source"
tar -xJf "$work/downloads/ffmpeg-9.0.2.tar.xz" -C "$work/source"

(
  cd "$work/source/zimg-release-3.0.6"
  ./autogen.sh
  PKG_CONFIG_PATH= PKG_CONFIG_LIBDIR="$prefix/lib/pkgconfig:$prefix/lib64/pkgconfig" \
    ./configure --prefix="$prefix" --libdir="$prefix/lib" --enable-shared --disable-static
  make -j2
  make install
)

cmake -S "$work/source/SVT-AV1-v4.2.0" -B "$work/build/svt-av1" -G Ninja \
  -DCMAKE_BUILD_TYPE=Release -DCMAKE_INSTALL_PREFIX="$prefix" \
  -DCMAKE_INSTALL_LIBDIR=lib -DBUILD_SHARED_LIBS=ON -DBUILD_APPS=OFF \
  -DBUILD_TESTING=OFF -DNATIVE=OFF -DSVT_AV1_LTO=OFF \
  -DEXCLUDE_HASH=ON -DREPRODUCIBLE_BUILDS=ON
cmake --build "$work/build/svt-av1" --parallel 2
cmake --install "$work/build/svt-av1"

(
  cd "$work/source/ffmpeg-9.0.2"
  PKG_CONFIG_PATH= PKG_CONFIG_LIBDIR="$prefix/lib/pkgconfig:$prefix/lib64/pkgconfig" \
    ./configure \
      --prefix="$prefix" --libdir="$prefix/lib" --shlibdir="$prefix/lib" \
      --enable-shared --disable-static --disable-autodetect --disable-doc \
      --disable-debug --disable-network --disable-avdevice --disable-ffplay \
      --enable-libsvtav1 --enable-libzimg --enable-libaom \
      --extra-cflags="-I$prefix/include" --extra-ldflags="-L$prefix/lib -Wl,-rpath,$prefix/lib" \
      --extra-libs='-lpthread -lm'
  make -j2
  make install
)

PKG_CONFIG_PATH= PKG_CONFIG_LIBDIR="$prefix/lib/pkgconfig:$prefix/lib64/pkgconfig" \
  pkg-config --modversion zimg SvtAv1Enc aom
LD_LIBRARY_PATH="$prefix/lib:$prefix/lib64" "$prefix/bin/ffmpeg" -version
LD_LIBRARY_PATH="$prefix/lib:$prefix/lib64" "$prefix/bin/ffmpeg" -hide_banner -encoders 2>&1 | grep -F libsvtav1
LD_LIBRARY_PATH="$prefix/lib:$prefix/lib64" "$prefix/bin/ffmpeg" -hide_banner -filters 2>&1 | grep -E '(^| )zscale( |$)'
