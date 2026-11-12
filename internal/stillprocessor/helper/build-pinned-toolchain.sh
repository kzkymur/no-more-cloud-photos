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

download lcms2-2.14.tar.gz \
  https://github.com/mm2/Little-CMS/releases/download/lcms2.14/lcms2-2.14.tar.gz \
  28474ea6f6591c4d4cee972123587001a4e6e353412a41b3e9e82219818d5740
download LibRaw-0.22.2.tar.gz \
  https://www.libraw.org/data/LibRaw-0.22.2.tar.gz \
  de86b035655accff8d4010f1a221fdf50d353cb7b1422ba26f14a0db92612cfa
download libheif-1.23.5.tar.gz \
  https://github.com/strukturag/libheif/releases/download/v1.23.5/libheif-1.23.5.tar.gz \
  fd9036064c4432f0550d15072ddf34956a248279ee9aeaff0fba3fa0f77d8f1a
download libwebp-1.6.0.tar.gz \
  https://storage.googleapis.com/downloads.webmproject.org/releases/webp/libwebp-1.6.0.tar.gz \
  e4ab7009bf0629fd11982d4c2aa83964cf244cffba7347ecd39019a9e38c4564
download vips-8.18.7.tar.xz \
  https://github.com/libvips/libvips/releases/download/v8.18.7/vips-8.18.7.tar.xz \
  5baaead3b0bb20ffdb9e9ff09aa9fda08620923df77b63b436654cb5e0b3bf94

rm -rf "$work/source/aom" "$work/source/lcms2-2.14" "$work/source/LibRaw-0.22.2" \
  "$work/source/libheif-1.23.5" "$work/source/libwebp-1.6.0" "$work/source/vips-8.18.7"
mkdir -p "$work/source/aom"
git -C "$work/source/aom" init --quiet
git -C "$work/source/aom" remote add origin https://aomedia.googlesource.com/aom
git -C "$work/source/aom" fetch --quiet --depth=1 origin 615b5f541e4434aebd993036bc97ebc1a77ebc25
git -C "$work/source/aom" checkout --quiet --detach FETCH_HEAD
test "$(git -C "$work/source/aom" rev-parse HEAD)" = 615b5f541e4434aebd993036bc97ebc1a77ebc25
test "$(git -C "$work/source/aom" archive --format=tar HEAD | sha256sum | cut -d' ' -f1)" = \
  eb0bfa625cd17849be2e17ffd38bf8e1dc67b7c7787e7152250a4075d05f245f
tar -xzf "$work/downloads/lcms2-2.14.tar.gz" -C "$work/source"
tar -xzf "$work/downloads/LibRaw-0.22.2.tar.gz" -C "$work/source"
tar -xzf "$work/downloads/libheif-1.23.5.tar.gz" -C "$work/source"
tar -xzf "$work/downloads/libwebp-1.6.0.tar.gz" -C "$work/source"
tar -xJf "$work/downloads/vips-8.18.7.tar.xz" -C "$work/source"

# Every pinned shared object carries DT_RUNPATH=$prefix/lib so its transitive
# pinned dependencies resolve inside the prefix regardless of the caller's
# environment (processrunner executes helpers with an empty environment).
# --enable-new-dtags emits RUNPATH, not the deprecated RPATH.
pinned_ldflags="-Wl,--enable-new-dtags -Wl,-rpath,$prefix/lib"
cmake_rpath=(-DCMAKE_INSTALL_RPATH="$prefix/lib" -DCMAKE_INSTALL_RPATH_USE_LINK_PATH=OFF
  -DCMAKE_SHARED_LINKER_FLAGS=-Wl,--enable-new-dtags -DCMAKE_EXE_LINKER_FLAGS=-Wl,--enable-new-dtags)

cmake -S "$work/source/aom" -B "$work/build/aom" -G Ninja \
  -DCMAKE_BUILD_TYPE=Release -DCMAKE_INSTALL_PREFIX="$prefix" \
  -DBUILD_SHARED_LIBS=ON -DENABLE_DOCS=OFF -DENABLE_EXAMPLES=OFF \
  -DENABLE_TESTS=OFF -DENABLE_TOOLS=OFF "${cmake_rpath[@]}"
cmake --build "$work/build/aom" --parallel 2
cmake --install "$work/build/aom"

(
  cd "$work/source/lcms2-2.14"
  ./configure --prefix="$prefix" --enable-shared --disable-static LDFLAGS="$pinned_ldflags"
  make -j2
  make install
)

export PKG_CONFIG_PATH="$prefix/lib/pkgconfig:$prefix/lib64/pkgconfig${PKG_CONFIG_PATH:+:$PKG_CONFIG_PATH}"
export LD_LIBRARY_PATH="$prefix/lib:$prefix/lib64${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"

(
  cd "$work/source/LibRaw-0.22.2"
  ./configure --prefix="$prefix" --enable-shared --disable-static \
    --disable-examples --disable-openmp --enable-jpeg --enable-lcms LDFLAGS="$pinned_ldflags"
  make -j2
  make install
)

cmake -S "$work/source/libheif-1.23.5" -B "$work/build/libheif" -G Ninja \
  -DCMAKE_BUILD_TYPE=Release -DCMAKE_INSTALL_PREFIX="$prefix" \
  -DBUILD_SHARED_LIBS=ON -DBUILD_TESTING=OFF -DWITH_EXAMPLES=OFF \
  -DENABLE_PLUGIN_LOADING=OFF -DWITH_AOM_DECODER=ON -DWITH_AOM_ENCODER=ON \
  -DWITH_LIBDE265=ON -DWITH_X265=OFF -DWITH_DAV1D=OFF -DWITH_RAV1E=OFF \
  -DWITH_SvtEnc=OFF "${cmake_rpath[@]}"
cmake --build "$work/build/libheif" --parallel 2
cmake --install "$work/build/libheif"

# Pinned libwebp/libwebpmux/libwebpdemux/libsharpyuv 1.6.0 MUST exist before
# vips is configured, so -Dwebp=enabled links them instead of distro
# libwebp-dev with the same SONAMEs. The animation build inherits this copy.
cmake -S "$work/source/libwebp-1.6.0" -B "$work/build/libwebp" -G Ninja \
  -DCMAKE_BUILD_TYPE=Release -DCMAKE_INSTALL_PREFIX="$prefix" -DCMAKE_INSTALL_LIBDIR=lib \
  -DBUILD_SHARED_LIBS=ON -DWEBP_BUILD_ANIM_UTILS=OFF \
  -DWEBP_BUILD_CWEBP=OFF -DWEBP_BUILD_DWEBP=OFF -DWEBP_BUILD_EXTRAS=OFF \
  -DWEBP_BUILD_GIF2WEBP=OFF -DWEBP_BUILD_IMG2WEBP=OFF \
  -DWEBP_BUILD_LIBWEBPMUX=ON -DWEBP_BUILD_VWEBP=OFF \
  -DWEBP_BUILD_WEBPINFO=OFF -DWEBP_BUILD_WEBPMUX=OFF \
  -DWEBP_BUILD_WEBP_JS=OFF -DWEBP_BUILD_FUZZTEST=OFF \
  -DWEBP_ENABLE_SIMD=ON -DWEBP_USE_THREAD=ON -DWEBP_NEAR_LOSSLESS=ON \
  -DWEBP_ENABLE_SWAP_16BIT_CSP=OFF -DWEBP_ENABLE_WUNUSED_RESULT=ON "${cmake_rpath[@]}"
cmake --build "$work/build/libwebp" --parallel 2
cmake --install "$work/build/libwebp"
[[ "$(PKG_CONFIG_PATH= PKG_CONFIG_LIBDIR="$prefix/lib/pkgconfig:$prefix/lib64/pkgconfig" \
  pkg-config --modversion libwebp libwebpdemux libwebpmux libsharpyuv | sort -u)" == 1.6.0 ]]

meson setup "$work/build/vips" "$work/source/vips-8.18.7" \
  --prefix="$prefix" --libdir=lib --buildtype=release \
  -Ddeprecated=false -Dexamples=false -Dcplusplus=false -Dmodules=disabled \
  -Dintrospection=disabled -Dcfitsio=disabled -Dcgif=disabled -Dexif=enabled \
  -Dfftw=disabled -Dfontconfig=disabled -Darchive=disabled -Dheif=disabled \
  -Dimagequant=disabled -Djpeg=enabled -Duhdr=disabled -Djpeg-xl=disabled \
  -Dlcms=enabled -Dmagick=disabled -Dmatio=disabled -Dnifti=disabled \
  -Dopenexr=disabled -Dopenjpeg=disabled -Dopenslide=disabled -Dhighway=disabled \
  -Dorc=disabled -Dpangocairo=disabled -Dpdfium=disabled -Dpng=enabled \
  -Dpoppler=disabled -Dquantizr=disabled -Draw=disabled -Drsvg=disabled \
  -Dspng=disabled -Dtiff=disabled -Dwebp=enabled -Dzlib=enabled \
  -Dnsgif=false -Dppm=false -Danalyze=false -Dradiance=false
meson compile -C "$work/build/vips" -j 2
meson install -C "$work/build/vips"
# meson's install-time depfixer deletes every rpath entry equal to an external
# dependency's library directory, so -Wl,-rpath,$prefix/lib in link args never
# survives into the installed libvips (all deps live in $prefix/lib). meson has
# no setup option for install_rpath, so set DT_RUNPATH (patchelf's default,
# not DT_RPATH) on the installed object; the closure gate below verifies it.
vips_objects=("$prefix"/lib/libvips.so.42.*.*)
[[ ${#vips_objects[@]} -eq 1 && -f "${vips_objects[0]}" && ! -L "${vips_objects[0]}" ]] ||
  { printf 'expected exactly one installed libvips object\n' >&2; exit 1; }
patchelf --set-rpath "$prefix/lib" "${vips_objects[0]}"

pkg-config --modversion vips libheif libraw aom lcms2 libwebp

# Closure gate: with an EMPTY environment (as processrunner runs helpers),
# every pinned shared object must carry RUNPATH=$prefix/lib and resolve each
# pinned SONAME from the prefix. Distro copies with the same SONAME are a
# failure, not a fallback.
# Each pinned library must resolve to exactly one regular (non-symlink)
# versioned object; counting all matches would let SONAME symlinks mask a
# missing library.
pinned_objects=()
shopt -s nullglob
for library in aom lcms2 raw raw_r heif webp webpmux webpdemux sharpyuv vips; do
  regular=()
  for candidate in "$prefix"/lib/lib"$library".so.*; do
    [[ -f "$candidate" && ! -L "$candidate" ]] && regular+=("$candidate")
  done
  [[ ${#regular[@]} -eq 1 ]] ||
    { printf 'pinned library lib%s: want exactly one regular object, found %d\n' "$library" "${#regular[@]}" >&2; exit 1; }
  pinned_objects+=("${regular[0]}")
done
shopt -u nullglob
for object in "${pinned_objects[@]}"; do
  runpath=$(readelf -d "$object" | sed -n 's/.*(RUNPATH).*\[\(.*\)\]/\1/p')
  # libtool may repeat the entry; every entry must be exactly the prefix.
  [[ -n "$runpath" && -z "$(tr ':' '\n' <<<"$runpath" | grep -vFx "$prefix/lib")" ]] ||
    { printf '%s RUNPATH=%s, want only %s\n' "$object" "${runpath:-none}" "$prefix/lib" >&2; exit 1; }
  while read -r soname _ resolved _; do
    case "$soname" in
      libaom.so.*|liblcms2.so.*|libraw*.so.*|libheif.so.*|libwebp*.so.*|libsharpyuv.so.*|libvips.so.*)
        [[ "$resolved" == "$prefix/lib/"* ]] || { printf '%s resolves %s to %s outside %s\n' "$object" "$soname" "$resolved" "$prefix" >&2; exit 1; } ;;
    esac
  done < <(env -i /usr/bin/ldd "$object" | grep '=>')
done

shopt -s nullglob
aom_objects=("$prefix"/lib/libaom.so.*.*.* "$prefix"/lib64/libaom.so.*.*.*)
shopt -u nullglob
[[ ${#aom_objects[@]} -eq 1 && -f "${aom_objects[0]}" && ! -L "${aom_objects[0]}" ]]
aom_object=${aom_objects[0]#"$prefix"/}
read -r aom_object_sha256 _ < <(sha256sum "${aom_objects[0]}")
mkdir -p "$prefix/share/nmcp"
printf '%s\n' \
  'manifest_version=1' \
  'libaom_version=3.8.2' \
  'libaom_commit=615b5f541e4434aebd993036bc97ebc1a77ebc25' \
  'libaom_source_sha256=eb0bfa625cd17849be2e17ffd38bf8e1dc67b7c7787e7152250a4075d05f245f' \
  "libaom_object=$aom_object" \
  "libaom_object_sha256=$aom_object_sha256" \
  >"$prefix/share/nmcp/still-toolchain.manifest"
# Separate manifest: still-toolchain.manifest v1 is validated field-exactly by
# the video helper's inherited-toolchain check and must stay unchanged.
printf '%s\n' \
  'manifest_version=1' \
  'libwebp_version=1.6.0' \
  'libwebp_source_sha256=e4ab7009bf0629fd11982d4c2aa83964cf244cffba7347ecd39019a9e38c4564' \
  >"$prefix/share/nmcp/still-webp.manifest"
for lib in libwebp libwebpmux libwebpdemux libsharpyuv; do
  shopt -s nullglob
  objects=("$prefix"/lib/"$lib".so.*.*.*)
  shopt -u nullglob
  [[ ${#objects[@]} -eq 1 && ! -L "${objects[0]}" ]]
  read -r digest _ < <(sha256sum "${objects[0]}")
  printf '%s_object=%s\n%s_object_sha256=%s\n' "$lib" "${objects[0]#"$prefix"/}" "$lib" "$digest" \
    >>"$prefix/share/nmcp/still-webp.manifest"
done
