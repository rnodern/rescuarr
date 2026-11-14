#!/usr/bin/env bash
set -euo pipefail

lab_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_dir="$(cd -- "$lab_dir/../.." && pwd)"
if [[ $# -lt 1 || $# -gt 2 || -e "$1" || -L "$1" ]]; then
  printf 'Usage: %s NEW_REPORT.json [sonarr|radarr]/[import|reject-preserve|reject-remove|reject-lost-response|replace-terminal] [or sonarr/v5-id-import|sonarr/v5-empty-pack|sonarr/v5-adapter-pack]\n' "$0" >&2
  exit 2
fi
scenario="${2:-}"
case "$scenario" in
  ""|sonarr/import|sonarr/v5-id-import|sonarr/v5-empty-pack|sonarr/v5-adapter-pack|radarr/import|sonarr/reject-preserve|radarr/reject-preserve|sonarr/reject-remove|radarr/reject-remove|sonarr/reject-lost-response|radarr/reject-lost-response|sonarr/replace-terminal|radarr/replace-terminal) ;;
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
  if [[ "$scenario" == sonarr/v5-id-import || "$scenario" == sonarr/v5-empty-pack || "$scenario" == sonarr/v5-adapter-pack ]]; then
    compose+=(--file "$lab_dir/compose.sonarr-v5.yaml")
    services=(sonarr sab)
  fi
  if [[ "$scenario" == sonarr/v5-adapter-pack ]]; then
    (cd "$repo_dir" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o "$run_dir/go-pack-probe" ./test/lab/go-pack-probe)
    compose+=(--file "$lab_dir/compose.go-pack-probe.yaml")
  fi
  scenario_args+=("$scenario")
fi
# The source fixture uses the probe's pinned image. Pull it once. Fetch each
# required image separately so throttling one registry cannot interrupt others.
pull_services=(probe "${services[@]}")
images="$("${compose[@]}" config --images "${pull_services[@]}")"
for service in "${pull_services[@]}"; do
  for delay in 5 20 45 0; do
    if "${compose[@]}" pull --policy missing "$service" >"$run_dir/pull.log" 2>&1; then
      cat "$run_dir/pull.log" >&2
      break
    fi
    cat "$run_dir/pull.log" >&2
    if [[ "$delay" == 0 ]] || ! grep -qiE 'toomanyrequests|too many requests' "$run_dir/pull.log"; then
      exit 1
    fi
    printf 'Registry throttled the %s image. Retrying in %s seconds.\n' "$service" "$delay" >&2
    sleep "$delay"
  done
done
if [[ -n "$scenario" ]]; then
  services+=(source)
fi
"${compose[@]}" up -d --no-build --pull never "${services[@]}" >&2
(set -o noclobber
 "${compose[@]}" run --rm --no-deps --pull never -T probe python -B probe.py "$source_sha" "$images" ${scenario_args[@]+"${scenario_args[@]}"} > "$1")
