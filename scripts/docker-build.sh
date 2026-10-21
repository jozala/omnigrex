#!/bin/sh
set -eu

# Local builds retain the ordinary Docker CLI behavior. CI restores this
# directory before the job and saves it after all image builds finish.
if [ -z "${OMNIGREX_BUILDKIT_CACHE_DIR:-}" ]; then
  if [ "${OMNIGREX_BUILDKIT_CACHE_ONLY:-0}" = 1 ]; then
    exec docker buildx build --output type=cacheonly "$@"
  fi
  exec docker build "$@"
fi

file=Dockerfile
target=default
previous=
for argument do
  if [ "$previous" = file ]; then file=$argument; previous=; continue; fi
  if [ "$previous" = target ]; then target=$argument; previous=; continue; fi
  case "$argument" in
    --file|-f) previous=file ;;
    --target) previous=target ;;
    --file=*) file=${argument#--file=} ;;
    --target=*) target=${argument#--target=} ;;
  esac
done
test -z "$previous"
cache_tag="$(printf '%s-%s' "$file" "$target" | tr '/.' '--')"
mkdir -p "$OMNIGREX_BUILDKIT_CACHE_DIR"
if [ "${OMNIGREX_BUILDKIT_CACHE_ONLY:-0}" = 1 ]; then
  output=cacheonly
else
  output=docker
fi
if [ -n "${OMNIGREX_BUILDKIT_PUSH_IMAGE:-}" ]; then
  test "$output" = docker
  : "${OMNIGREX_BUILDKIT_LOCAL_IMAGE:?set the local image name for a dual export}"
  output="docker,name=$OMNIGREX_BUILDKIT_LOCAL_IMAGE"
  set -- --output "type=image,name=$OMNIGREX_BUILDKIT_PUSH_IMAGE,push=true,registry.insecure=true" "$@"
fi
# Reset removes blobs from replaced manifests while retaining other target tags.
exec docker buildx build --output "type=$output" \
  --cache-from "type=local,src=$OMNIGREX_BUILDKIT_CACHE_DIR,tag=$cache_tag" \
  --cache-to "type=local,dest=$OMNIGREX_BUILDKIT_CACHE_DIR,tag=$cache_tag,mode=max,reset=true,ignore-error=true" \
  "$@"
