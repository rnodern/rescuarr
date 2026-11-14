#!/usr/bin/env bash
set -euo pipefail

architecture=${1:?Pass amd64 or arm64}
version=${RESCUARR_VERSION:-candidate}
case "$architecture:$(uname -m)" in
  amd64:x86_64|arm64:aarch64) ;;
  *) echo "Runner does not natively match linux/$architecture" >&2; exit 1 ;;
esac
[[ "$version" =~ ^[A-Za-z0-9][A-Za-z0-9.+_-]{0,127}$ ]]
git diff --quiet HEAD --
revision=$(git rev-parse HEAD)
output="dist/oci-$architecture"
mkdir -p dist
mkdir "$output"
archive="$output/rescuarr-linux-$architecture.oci.tar"
export RESCUARR_IMAGE="rescuarr:candidate-$architecture"
export RESCUARR_HOST_PORT="${RESCUARR_HOST_PORT:-18787}"
unset RESCUARR_PUBLIC_URL RESCUARR_BASE_PATH RESCUARR_TRUSTED_PROXIES
unset COMPOSE_FILE COMPOSE_PROJECT_NAME COMPOSE_PROFILES COMPOSE_ENV_FILES COMPOSE_REMOVE_ORPHANS COMPOSE_IGNORE_ORPHANS


scratch=$(mktemp -d)
project="rescuarr-smoke-$architecture-$(basename "$scratch" | tr '[:upper:]' '[:lower:]' | tr -cd '[:alnum:]')"
compose=(docker compose --env-file /dev/null --file compose.yaml --project-name "$project")
cleanup() {
  local status=$?
  "${compose[@]}" down --volumes --remove-orphans || status=1
  rm -rf "$scratch" || status=1
  exit "$status"
}
trap cleanup EXIT
git archive HEAD go.mod go.sum cmd internal deploy/container deploy/licenses |
  docker buildx build --platform "linux/$architecture" --file deploy/container/Dockerfile \
    --tag "$RESCUARR_IMAGE" --provenance=false --sbom=false \
    --build-arg "RESCUARR_VERSION=$version" --build-arg "RESCUARR_REVISION=$revision" \
    --output "type=oci,dest=$archive,oci-mediatypes=true" -
skopeo --insecure-policy copy "oci-archive:$archive" "docker-daemon:$RESCUARR_IMAGE"

"${compose[@]}" config --quiet
"${compose[@]}" up --detach --no-build --pull never
python3 scripts/packaging/container.py "$architecture" "$version" "$revision" "$archive" "$project" "$scratch"
