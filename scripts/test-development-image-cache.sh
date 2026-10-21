#!/bin/sh
set -eu
image="${OMNIGREX_DEVELOPMENT_IMAGE:-omnigrex/development:dev}"
cache_image="${OMNIGREX_DEVELOPMENT_CACHE_IMAGE:-omnigrex/development-cache:dev}"
sh scripts/docker-build.sh --platform linux/amd64 --file agent/environments/omnigrex/Dockerfile \
  --target cache-qualification --build-arg "OMNIGREX_DEVELOPMENT_IMAGE=$image" --tag "$cache_image" .
docker run --rm --platform linux/amd64 --network none --read-only \
  --tmpfs /tmp:rw,exec,nosuid,nodev,size=1g,mode=1777 \
  --env OMNIGREX_DEVELOPMENT_CACHE_TEST=1 "$cache_image"
