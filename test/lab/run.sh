#!/usr/bin/env bash
set -euo pipefail

lab_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_dir="$(cd -- "$lab_dir/../.." && pwd)"
if [[ $# -ne 1 || -e "$1" || -L "$1" ]]; then
  printf 'Usage: %s NEW_REPORT.json\n' "$0" >&2
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
  "${compose[@]}" down --volumes --remove-orphans >&2 || true
  rmdir "$run_dir"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

source_sha="$(git -C "$repo_dir" rev-parse HEAD)"
images="$("${compose[@]}" config --images)"
"${compose[@]}" pull >&2
"${compose[@]}" up -d --no-build sonarr radarr sab >&2
(set -o noclobber
 "${compose[@]}" run --rm --no-deps -T probe python -B probe.py "$source_sha" "$images" > "$1")
