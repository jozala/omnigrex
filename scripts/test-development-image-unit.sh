#!/bin/sh
set -eu
image="${OMNIGREX_DEVELOPMENT_IMAGE:-omnigrex/development:dev}"
unit_image="${OMNIGREX_DEVELOPMENT_UNIT_IMAGE:-omnigrex/development-unit:dev}"
sh scripts/docker-build.sh --platform linux/amd64 --file agent/environments/omnigrex/Dockerfile \
  --target repository-qualification --build-context development-docs=./docs \
  --build-arg "OMNIGREX_DEVELOPMENT_IMAGE=$image" --tag "$unit_image" .
# No host Docker socket or secrets: unit tests use their own controlled fixtures.
docker run --rm --platform linux/amd64 --cpus 2 --memory 4g --pids-limit 128 "$unit_image" "$@"
