# shellcheck shell=bash
# Failpoint lane state for scripts/infra-e2e-env, sourced (not executed).
# Each service's point lives in its own file, so an empty value for one
# service can never shift into the other (a shared space-delimited line did:
# `read` strips leading IFS whitespace). An absent file means "none".
#
# Requires: $state_dir.

# shellcheck disable=SC2154 # state_dir is set by the sourcing script
failpoint_file_for() {
  case "$1" in
    api|worker) printf '%s/failpoint-%s' "$state_dir" "$1" ;;
    *) echo "infra-e2e: unknown failpoint service: $1" >&2; return 2 ;;
  esac
}

# Print the stored point for SERVICE ("" when none).
failpoint_get() {
  local file value=""
  file=$(failpoint_file_for "$1") || return
  if [ -f "$file" ]; then IFS= read -r value <"$file" || true; fi
  printf '%s' "$value"
}

# Store POINT for SERVICE; an empty POINT clears it.
failpoint_set() {
  local file
  file=$(failpoint_file_for "$1") || return
  if [ -z "$2" ]; then rm -f -- "$file"; else printf '%s\n' "$2" >"$file"; fi
}

failpoint_clear_all() {
  rm -f -- "$state_dir/failpoint-api" "$state_dir/failpoint-worker" "$state_dir/failpoints"
}
