#!/bin/sh
set -eu

builder="${OMNIGREX_CI_BUILDER:-omnigrex-ci}"
network="${OMNIGREX_CI_NETWORK:-omnigrex-ci-build}"
registry="${OMNIGREX_CI_REGISTRY:-omnigrex-ci-registry}"

case "${1:-}" in
  start)
    docker network create "$network" >/dev/null
    docker run -d --name "$registry" --network "$network" \
      registry:2@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373 >/dev/null
    ready=0
    for attempt in 1 2 3 4 5 6 7 8 9 10; do
      if docker exec "$registry" wget -q -O /dev/null http://127.0.0.1:5000/v2/; then
        ready=1
        break
      fi
      sleep 1
    done
    test "$ready" = 1
    config="$(mktemp)"
    trap 'rm -f "$config"' 0
    printf '[registry."%s:5000"]\n  http = true\n' "$registry" > "$config"
    docker buildx create --name "$builder" --driver docker-container \
      --driver-opt "image=moby/buildkit@sha256:cec9f139f45e93c5c69c60f8b07cfad9f43f4ef6b6a6cd917527fea5ff2e3dea" \
      --driver-opt "network=$network" --buildkitd-config "$config" --use >/dev/null
    docker buildx inspect --bootstrap >/dev/null
    ;;
  stop)
    docker buildx rm "$builder"
    docker rm -f "$registry" >/dev/null
    docker network rm "$network" >/dev/null
    ;;
  *)
    printf 'usage: %s start|stop\n' "$0" >&2
    exit 2
    ;;
esac
