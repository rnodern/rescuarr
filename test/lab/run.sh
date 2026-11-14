#!/usr/bin/env bash
set -euo pipefail

lab_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_dir="$(cd -- "$lab_dir/../.." && pwd)"
if [[ $# -lt 1 || $# -gt 2 || -e "$1" || -L "$1" ]]; then
  printf 'Usage: %s NEW_REPORT.json [sonarr|radarr]/[import|reject-preserve|reject-remove|reject-lost-response|replace-terminal]\n' "$0" >&2
  exit 2
fi
scenario="${2:-}"
case "$scenario" in
  ""|sonarr/import|radarr/import|sonarr/reject-preserve|radarr/reject-preserve|sonarr/reject-remove|radarr/reject-remove|sonarr/reject-lost-response|radarr/reject-lost-response|sonarr/replace-terminal|radarr/replace-terminal) ;;
  *) printf 'Unknown lab scenario.\n' >&2; exit 2 ;;
esac

if [[ -n "$(git -C "$repo_dir" status --porcelain --untracked-files=all -- test/lab .github/workflows/upstream-lab.yml)" ]]; then
  printf 'Commit the lab and workflow files before running an evidence capture.\n' >&2
  exit 2
fi

# Ignore caller-supplied Compose project, file, profile and environment settings.
unset COMPOSE_FILE COMPOSE_PROJECT_NAME COMPOSE_PROFILES COMPOSE_ENV_FILES COMPOSE_REMOVE_ORPHANS COMPOSE_IGNORE_ORPHANS

if ! docker compose version >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; then
  printf 'The lab needs a reachable Docker Engine and Docker Compose v2.\n' >&2
  exit 2
fi

run_dir="$(mktemp -d "${TMPDIR:-/tmp}/rescuarr-lab.XXXXXXXX")"
project="rescuarr-lab-$(basename "$run_dir" | tr '[:upper:]' '[:lower:]' | cut -d. -f2)"
compose=(docker compose --env-file /dev/null --project-name "$project" --file "$lab_dir/compose.yaml")
cleanup() {
  local status=$?
  if ! "${compose[@]}" down --volumes --remove-orphans >&2; then
    printf 'Cleanup failed for disposable project %s.\n' "$project" >&2
    status=1
  fi
  rm -rf -- "$run_dir" || status=1
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

source_sha="$(git -C "$repo_dir" rev-parse HEAD)"
services=(sonarr radarr sab)
scenario_args=()
if [[ -n "$scenario" ]]; then
  if ! command -v openssl >/dev/null || ! command -v ffmpeg >/dev/null; then
    printf 'Lab scenarios need OpenSSL and FFmpeg to generate disposable inputs.\n' >&2
    exit 2
  fi
  bash "$lab_dir/seed.sh" "$run_dir" >&2
  export RESCUARR_LAB_RUN_DIR="$run_dir"
  compose+=(--file "$lab_dir/compose.scenarios.yaml")
  services+=(source)
  scenario_args+=("$scenario")
fi
images="$("${compose[@]}" config --images)"
for delay in 3 12 0; do
  if "${compose[@]}" pull >"$run_dir/pull.log" 2>&1; then
    cat "$run_dir/pull.log" >&2
    break
  fi
  cat "$run_dir/pull.log" >&2
  if [[ "$delay" == 0 ]] || ! grep -qiE 'toomanyrequests|too many requests' "$run_dir/pull.log"; then
    exit 1
  fi
  sleep "$delay"
done
"${compose[@]}" up -d --no-build "${services[@]}" >&2
(set -o noclobber
 "${compose[@]}" run --rm --no-deps -T probe python -B probe.py "$source_sha" "$images" ${scenario_args[@]+"${scenario_args[@]}"} > "$1")
