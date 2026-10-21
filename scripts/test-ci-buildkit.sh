#!/bin/sh
set -eu

builder="${OMNIGREX_CI_BUILDER:-omnigrex-ci}"
registry="${OMNIGREX_CI_REGISTRY:-omnigrex-ci-registry}"
base_image="omnigrex/ci-chain-base:ci-$$"
child_image="omnigrex/ci-chain-child:ci-$$"
started=0
fixture="$(mktemp -d)"
cleanup() {
  if [ "$started" = 1 ]; then sh scripts/ci-buildkit-registry.sh stop; fi
  rm -rf "$fixture"
}
trap cleanup 0
if ! docker buildx inspect "$builder" >/dev/null 2>&1; then
  started=1
  sh scripts/ci-buildkit-registry.sh start
fi
docker buildx use "$builder"

printf 'source-built base\n' > "$fixture/base.txt"
printf 'FROM scratch\nCOPY base.txt /base.txt\n' > "$fixture/base.Dockerfile"
OMNIGREX_BUILDKIT_CACHE_DIR="$fixture/cache" \
  OMNIGREX_BUILDKIT_LOCAL_IMAGE="$base_image" \
  OMNIGREX_BUILDKIT_PUSH_IMAGE="$registry:5000/$base_image" sh scripts/docker-build.sh \
  --file "$fixture/base.Dockerfile" "$fixture"
docker image inspect "$base_image" >/dev/null

printf 'FROM %s:5000/%s\nCOPY child.txt /child.txt\n' "$registry" "$base_image" > "$fixture/child.Dockerfile"
printf 'child 1\n' > "$fixture/child.txt"
OMNIGREX_BUILDKIT_CACHE_DIR="$fixture/cache" \
  OMNIGREX_BUILDKIT_LOCAL_IMAGE="$child_image" \
  OMNIGREX_BUILDKIT_PUSH_IMAGE="$registry:5000/$child_image" sh scripts/docker-build.sh \
  --file "$fixture/child.Dockerfile" "$fixture"
docker tag "$child_image" "$registry:5000/$child_image"
container="$(docker create "$registry:5000/$child_image" /noop)"
docker cp "$container:/base.txt" - | tar -xO | grep -qx 'source-built base'
docker rm "$container" >/dev/null

printf 'FROM %s:5000/%s\nCOPY base.txt /grandchild.txt\n' "$registry" "$child_image" > "$fixture/grandchild.Dockerfile"
OMNIGREX_BUILDKIT_CACHE_DIR="$fixture/cache" sh scripts/docker-build.sh \
  --file "$fixture/grandchild.Dockerfile" --tag "omnigrex/ci-chain-grandchild:ci-$$" "$fixture"

# Replacing a tag must retain the base-image cache but discard older child blobs.
for version in 2 3 4; do
  printf 'child %s\n' "$version" > "$fixture/child.txt"
  OMNIGREX_BUILDKIT_CACHE_DIR="$fixture/cache" sh scripts/docker-build.sh \
    --file "$fixture/child.Dockerfile" --tag "$child_image" "$fixture"
  count="$(find "$fixture/cache/blobs/sha256" -type f | wc -l | tr -d ' ')"
  if [ "$version" = 3 ]; then previous="$count"; fi
  if [ "$version" = 4 ]; then test "$count" -le "$previous"; fi
done
grep -q 'base-Dockerfile-default' "$fixture/cache/index.json"
grep -q 'child-Dockerfile-default' "$fixture/cache/index.json"
