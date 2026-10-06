#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 2 || "$1" != /* || "$2" != /* ]]; then
  printf 'usage: %s ABSOLUTE_PREFIX ABSOLUTE_WORK\n' "$0" >&2
  exit 2
fi

prefix=$1
work=$2
mkdir -p "$prefix" "$work/downloads" "$work/source" "$work/build"
script_dir=$(cd -- "$(dirname -- "$0")" && pwd)

read -r inherited_aom_object inherited_aom_digest < <(
  "$script_dir/validate-inherited-toolchain.sh" "$prefix"
)

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
      --disable-gpl --disable-nonfree \
      --enable-libsvtav1 --enable-libzimg --enable-libaom \
      --extra-cflags="-I$prefix/include" \
      --extra-ldflags="-L$prefix/lib -Wl,--disable-new-dtags,-rpath,$prefix/lib" \
      --extra-libs='-lpthread -lm'
  make -j2
  make install
)

pkg_env=(env PKG_CONFIG_PATH= PKG_CONFIG_LIBDIR="$prefix/lib/pkgconfig:$prefix/lib64/pkgconfig")
[[ "$("${pkg_env[@]}" pkg-config --modversion zimg)" == 3.0.6 ]]
[[ "$("${pkg_env[@]}" pkg-config --modversion SvtAv1Enc)" == 4.2.0 ]]
read -r checked_aom_object checked_aom_digest < <(
  "$script_dir/validate-inherited-toolchain.sh" "$prefix"
)
[[ "$checked_aom_object" == "$inherited_aom_object" && "$checked_aom_digest" == "$inherited_aom_digest" ]]
mkdir -p "$prefix/share/nmcp"
printf '%s\n' \
  'manifest_version=1' \
  'ffmpeg_version=9.0.2' \
  'ffmpeg_source_sha256=8c3850283eb25fa026482078a04051e0be17347b09ef81a0849bec15a96e002e' \
  'svt_av1_version=4.2.0' \
  'svt_av1_source_sha256=c7b13c4a84bd3751aa35fcc72be13e6875467e7c2216879251a486e5b1e4e740' \
  'zimg_version=3.0.6' \
  'zimg_source_sha256=be89390f13a5c9b2388ce0f44a5e89364a20c1c57ce46d382b1fcc3967057577' \
  'libaom_version=3.8.2' \
  'libaom_commit=615b5f541e4434aebd993036bc97ebc1a77ebc25' \
  'libaom_source_sha256=eb0bfa625cd17849be2e17ffd38bf8e1dc67b7c7787e7152250a4075d05f245f' \
  "libaom_object=$checked_aom_object" \
  "libaom_object_sha256=$checked_aom_digest" \
  'configure=shared,no-autodetect,no-network,no-gpl,no-nonfree,libsvtav1,libzimg,libaom,rpath-pinned' \
  >"$prefix/share/nmcp/video-toolchain.manifest"
LD_LIBRARY_PATH="$prefix/lib:$prefix/lib64" "$prefix/bin/ffmpeg" -version
LD_LIBRARY_PATH="$prefix/lib:$prefix/lib64" "$prefix/bin/ffmpeg" -buildconf 2>&1 | grep -F -- '--disable-nonfree'
LD_LIBRARY_PATH="$prefix/lib:$prefix/lib64" "$prefix/bin/ffmpeg" -hide_banner -encoders 2>&1 | grep -F libsvtav1
LD_LIBRARY_PATH="$prefix/lib:$prefix/lib64" "$prefix/bin/ffmpeg" -hide_banner -filters 2>&1 | grep -E '(^| )zscale( |$)'
loaded_aom=''
while IFS= read -r dependency; do
  if [[ "$dependency" =~ ^[[:space:]]*libaom\.so\.[^[:space:]]+[[:space:]]*' =>'[[:space:]]*([^[:space:]]+) ]]; then
    loaded_aom=${BASH_REMATCH[1]}
  fi
done < <(LD_LIBRARY_PATH="$prefix/lib:$prefix/lib64" ldd "$prefix/bin/ffmpeg")
[[ -n "$loaded_aom" && "$(realpath -e "$loaded_aom")" == "$(realpath -e "$prefix/$checked_aom_object")" ]]
