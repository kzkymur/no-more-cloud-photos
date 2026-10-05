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

download aom-3.8.2.tar.gz \
  https://aomedia.googlesource.com/aom/+archive/615b5f541e4434aebd993036bc97ebc1a77ebc25.tar.gz \
  d17dfcf2b2caf23a0fcbdcaf3253f7cbcc2c9206389e9a96bf96f831d28269bc
download lcms2-2.14.tar.gz \
  https://github.com/mm2/Little-CMS/releases/download/lcms2.14/lcms2-2.14.tar.gz \
  28474ea6f6591c4d4cee972123587001a4e6e353412a41b3e9e82219818d5740
download LibRaw-0.22.2.tar.gz \
  https://www.libraw.org/data/LibRaw-0.22.2.tar.gz \
  de86b035655accff8d4010f1a221fdf50d353cb7b1422ba26f14a0db92612cfa
download libheif-1.23.5.tar.gz \
  https://github.com/strukturag/libheif/releases/download/v1.23.5/libheif-1.23.5.tar.gz \
  fd9036064c4432f0550d15072ddf34956a248279ee9aeaff0fba3fa0f77d8f1a
download vips-8.18.7.tar.xz \
  https://github.com/libvips/libvips/releases/download/v8.18.7/vips-8.18.7.tar.xz \
  5baaead3b0bb20ffdb9e9ff09aa9fda08620923df77b63b436654cb5e0b3bf94

rm -rf "$work/source/aom" "$work/source/lcms2-2.14" "$work/source/LibRaw-0.22.2" \
  "$work/source/libheif-1.23.5" "$work/source/vips-8.18.7"
mkdir -p "$work/source/aom"
tar -xzf "$work/downloads/aom-3.8.2.tar.gz" -C "$work/source/aom"
tar -xzf "$work/downloads/lcms2-2.14.tar.gz" -C "$work/source"
tar -xzf "$work/downloads/LibRaw-0.22.2.tar.gz" -C "$work/source"
tar -xzf "$work/downloads/libheif-1.23.5.tar.gz" -C "$work/source"
tar -xJf "$work/downloads/vips-8.18.7.tar.xz" -C "$work/source"

cmake -S "$work/source/aom" -B "$work/build/aom" -G Ninja \
  -DCMAKE_BUILD_TYPE=Release -DCMAKE_INSTALL_PREFIX="$prefix" \
  -DBUILD_SHARED_LIBS=ON -DENABLE_DOCS=OFF -DENABLE_EXAMPLES=OFF \
  -DENABLE_TESTS=OFF -DENABLE_TOOLS=OFF
cmake --build "$work/build/aom" --parallel 2
cmake --install "$work/build/aom"

(
  cd "$work/source/lcms2-2.14"
  ./configure --prefix="$prefix" --enable-shared --disable-static
  make -j2
  make install
)

export PKG_CONFIG_PATH="$prefix/lib/pkgconfig:$prefix/lib64/pkgconfig${PKG_CONFIG_PATH:+:$PKG_CONFIG_PATH}"
export LD_LIBRARY_PATH="$prefix/lib:$prefix/lib64${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"

(
  cd "$work/source/LibRaw-0.22.2"
  ./configure --prefix="$prefix" --enable-shared --disable-static \
    --disable-examples --disable-openmp --enable-jpeg --enable-lcms
  make -j2
  make install
)

cmake -S "$work/source/libheif-1.23.5" -B "$work/build/libheif" -G Ninja \
  -DCMAKE_BUILD_TYPE=Release -DCMAKE_INSTALL_PREFIX="$prefix" \
  -DBUILD_SHARED_LIBS=ON -DBUILD_TESTING=OFF -DWITH_EXAMPLES=OFF \
  -DENABLE_PLUGIN_LOADING=OFF -DWITH_AOM_DECODER=ON -DWITH_AOM_ENCODER=ON \
  -DWITH_LIBDE265=ON -DWITH_X265=OFF -DWITH_DAV1D=OFF -DWITH_RAV1E=OFF \
  -DWITH_SvtEnc=OFF
cmake --build "$work/build/libheif" --parallel 2
cmake --install "$work/build/libheif"

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

pkg-config --modversion vips libheif libraw aom lcms2
