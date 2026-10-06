#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 1 || "$1" != /* ]]; then
  printf 'usage: %s ABSOLUTE_PREFIX\n' "$0" >&2
  exit 2
fi

prefix=$1
manifest="$prefix/share/nmcp/still-toolchain.manifest"
[[ -f "$manifest" && ! -L "$manifest" ]]
mapfile -t fields <"$manifest"
[[ ${#fields[@]} -eq 6 ]]
[[ "${fields[0]}" == 'manifest_version=1' ]]
[[ "${fields[1]}" == 'libaom_version=3.8.2' ]]
[[ "${fields[2]}" == 'libaom_commit=615b5f541e4434aebd993036bc97ebc1a77ebc25' ]]
[[ "${fields[3]}" == 'libaom_source_sha256=eb0bfa625cd17849be2e17ffd38bf8e1dc67b7c7787e7152250a4075d05f245f' ]]

aom_object=${fields[4]#libaom_object=}
aom_digest=${fields[5]#libaom_object_sha256=}
[[ "${fields[4]}" == "libaom_object=$aom_object" ]]
[[ "$aom_object" =~ ^lib(64)?/libaom\.so\.[0-9]+\.[0-9]+\.[0-9]+$ ]]
[[ "$aom_digest" =~ ^[0-9a-f]{64}$ ]]
[[ -f "$prefix/$aom_object" && ! -L "$prefix/$aom_object" ]]
printf '%s  %s\n' "$aom_digest" "$prefix/$aom_object" | sha256sum --check --status

pkg_env=(env PKG_CONFIG_PATH= PKG_CONFIG_LIBDIR="$prefix/lib/pkgconfig:$prefix/lib64/pkgconfig")
[[ "$("${pkg_env[@]}" pkg-config --modversion aom)" == 3.8.2 ]]
aom_pc_dir=$("${pkg_env[@]}" pkg-config --variable=pcfiledir aom)
aom_lib_dir=$("${pkg_env[@]}" pkg-config --variable=libdir aom)
[[ "$aom_pc_dir" == "$prefix/lib/pkgconfig" || "$aom_pc_dir" == "$prefix/lib64/pkgconfig" ]]
[[ "$aom_lib_dir" == "$prefix/lib" || "$aom_lib_dir" == "$prefix/lib64" ]]
[[ "$aom_object" == "${aom_lib_dir#"$prefix"/}/"* ]]

printf '%s\t%s\n' "$aom_object" "$aom_digest"
