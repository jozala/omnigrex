#!/bin/sh
set -eu

repository=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
cd "$repository"
container="website-build-${GITHUB_RUN_ID:-local}-${GITHUB_RUN_ATTEMPT:-$$}"
trap 'docker rm --force "$container" >/dev/null 2>&1 || true' EXIT

docker build --tag omnigrex/website-ci website
docker run --name "$container" \
  --env REPOSITORY_ROOT=/repository \
  --volume "$repository:/repository:ro" \
  omnigrex/website-ci npm run check

# This ignored directory contains only generated output owned by this build.
rm -rf website/dist
mkdir -p website/dist
docker cp "$container:/website/dist/." website/dist/
