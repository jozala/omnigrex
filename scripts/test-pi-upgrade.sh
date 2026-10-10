#!/bin/sh
set -eu
backend="${OMNIGREX_PI_BACKEND:-docker}"
case "$backend" in docker|microsandbox) ;; *) exit 2 ;; esac
namespace="pi-upgrade-$(date +%s)-$$"
source_volume="$namespace-source"
candidate_volume="$namespace-candidate"
restore_volume="$namespace-restore"
wrapper="$namespace-wrapper"
owned_source="omnigrex/pi-upgrade-source:$namespace"
owned_target="omnigrex/pi-upgrade-target:$namespace"
archives="$(mktemp -d)"
cleanup() {
  docker rm -f "$wrapper" >/dev/null 2>&1 || true
  docker volume rm "$source_volume" "$candidate_volume" "$restore_volume" >/dev/null 2>&1 || true
  docker image rm "$owned_source" "$owned_target" >/dev/null 2>&1 || true
  rm -rf "$archives"
}
trap cleanup EXIT HUP INT TERM
source="${OMNIGREX_PI_UPGRADE_SOURCE_IMAGE:-}"
target="${OMNIGREX_PI_UPGRADE_TARGET_IMAGE:-}"
artifact="${OMNIGREX_RUNTIME_PROFILE_COMPATIBILITY_RESULTS_FILE:-}"
restore_input=""
source_contract=""
if [ -n "${OMNIGREX_PI_UPGRADE_SOURCE_CONTRACT_FILE:-}" ]; then source_contract=/tmp/pi-source-contract.json; fi
if [ -n "${OMNIGREX_PI_UPGRADE_RESTORE_ARCHIVE:-}" ]; then restore_input=/tmp/pi-upgrade-input.tar; fi
if [ -n "$artifact" ]; then
  # Fixture aliases are executable evidence, never deployment qualification.
  : "${source:?artifact emission requires an exact source registry digest}"
  : "${target:?artifact emission requires an exact candidate registry digest}"
  case "$artifact" in /*) ;; *) exit 2 ;; esac
  test ! -e "$artifact"
fi
if [ -z "$source" ] && [ -z "$target" ]; then
  if [ "${OMNIGREX_PI_UPGRADE_USE_CACHED_FIXTURES:-0}" != 1 ]; then
    docker build --quiet --file agent/pi/Dockerfile --target upgrade-source --tag omnigrex/pi-upgrade-source:1.0.3 .
    docker build --quiet --file agent/pi/Dockerfile --target golden --tag omnigrex/pi:1.0.4 .
  fi
  source=omnigrex/pi-upgrade-source:1.0.3
  target=omnigrex/pi:1.0.4
else
  test -n "$source" && test -n "$target"
  case "$source" in *@sha256:*) ;; *) exit 2 ;; esac
  case "$target" in *@sha256:*) ;; *) exit 2 ;; esac
fi
# Keep fixture images addressable if another gate rebuilds the discovery tags.
docker tag "$source" "$owned_source"
docker tag "$target" "$owned_target"
source="$owned_source"
target="$owned_target"
for volume in "$source_volume" "$candidate_volume" "$restore_volume"; do docker volume create "$volume" >/dev/null; done
docker run --rm --user 0:0 --entrypoint /bin/sh \
  --mount "type=volume,src=$source_volume,dst=/data/source" \
  --mount "type=volume,src=$candidate_volume,dst=/data/candidate" \
  --mount "type=volume,src=$restore_volume,dst=/data/restore" \
  "$target" -ec 'chown 10001:10001 /data /data/source /data/candidate /data/restore'
source_local="$(docker image inspect "$source" --format '{{.Id}}')"
target_local="$(docker image inspect "$target" --format '{{.Id}}')"
arch="$(docker image inspect "$target" --format '{{.Architecture}}')"
test "$(docker image inspect "$source" --format '{{.Architecture}}')" = "$arch"
source_binding="${OMNIGREX_PI_UPGRADE_SOURCE_IMAGE:-}"
target_binding="${OMNIGREX_PI_UPGRADE_TARGET_IMAGE:-}"
if [ -z "${OMNIGREX_PI_UPGRADE_SOURCE_IMAGE:-}" ]; then
  # Fixture-only aliases use layer content, which survives save/load across
  # Docker's classic and containerd stores. These are never release artifacts.
  source_content="$(docker image inspect "$source" --format '{{json .RootFS.Layers}}' | shasum -a 256 | cut -d ' ' -f 1)"
  target_content="$(docker image inspect "$target" --format '{{json .RootFS.Layers}}' | shasum -a 256 | cut -d ' ' -f 1)"
  source_binding="registry.example/omnigrex/pi-upgrade@sha256:$source_content"
  target_binding="registry.example/omnigrex/pi-upgrade@sha256:$target_content"
fi
if [ "$backend" = microsandbox ]; then
  : "${OMNIGREX_MICROSANDBOX_KVM_GID:?set the approved Docker host KVM group}"
  test "$arch" = amd64
  docker build --quiet --file Dockerfile.microsandbox --target pi-qualification --tag omnigrex/pi-upgrade-qualification:native .
  docker create --name "$wrapper" --device /dev/kvm --group-add "$OMNIGREX_MICROSANDBOX_KVM_GID" \
    --memory 4g --memory-swap 4g --cpus 2 --pids-limit 1024 \
    --mount "type=volume,src=$source_volume,dst=/data/source" --mount "type=volume,src=$candidate_volume,dst=/data/candidate" --mount "type=volume,src=$restore_volume,dst=/data/restore" \
    --env OMNIGREX_PI_UPGRADE_INTEGRATION=1 --env OMNIGREX_PI_BACKEND=microsandbox --env OMNIGREX_PI_UPGRADE_ROOT=/data \
    --env "OMNIGREX_PI_UPGRADE_SOURCE_VERSION=${OMNIGREX_PI_UPGRADE_SOURCE_VERSION:-1.0.3}" --env "OMNIGREX_PI_UPGRADE_TARGET_VERSION=${OMNIGREX_PI_UPGRADE_TARGET_VERSION:-1.0.4}" \
    --env OMNIGREX_PI_UPGRADE_ARTIFACT=/tmp/pi-upgrade-result.json \
    --env "OMNIGREX_PI_UPGRADE_RESTORE_ARCHIVE=$restore_input" \
    --env "OMNIGREX_PI_UPGRADE_SOURCE_CONTRACT_FILE=$source_contract" \
    --env "OMNIGREX_PI_UPGRADE_SOURCE_IMAGE=${OMNIGREX_PI_UPGRADE_SOURCE_IMAGE:-}" --env "OMNIGREX_PI_UPGRADE_TARGET_IMAGE=${OMNIGREX_PI_UPGRADE_TARGET_IMAGE:-}" \
    omnigrex/pi-upgrade-qualification:native -test.v -test.run '^TestPiControlledStateUpgrade$' -test.timeout 10m >/dev/null
  docker image save --output "$archives/source.tar" "$source"
  chmod 0644 "$archives/source.tar"
  docker cp "$archives/source.tar" "$wrapper:/tmp/pi-upgrade-SOURCE.tar"
  docker image save --output "$archives/target.tar" "$target"
  chmod 0644 "$archives/target.tar"
  docker cp "$archives/target.tar" "$wrapper:/tmp/pi-upgrade-TARGET.tar"
else
  docker build --quiet --platform "linux/$arch" --file Dockerfile.pi-qualification --tag omnigrex/pi-upgrade-qualification:docker .
  gid="$(docker run --rm --user 0:0 --entrypoint /usr/bin/stat --mount type=bind,src=/var/run/docker.sock,dst=/var/run/docker.sock "$target" -c %g /var/run/docker.sock)"
  docker create --name "$wrapper" --group-add "$gid" --mount type=bind,src=/var/run/docker.sock,dst=/var/run/docker.sock \
    --mount "type=volume,src=$source_volume,dst=/data/source" --mount "type=volume,src=$candidate_volume,dst=/data/candidate" --mount "type=volume,src=$restore_volume,dst=/data/restore" \
    --env OMNIGREX_PI_UPGRADE_INTEGRATION=1 --env OMNIGREX_PI_BACKEND=docker --env OMNIGREX_PI_UPGRADE_ROOT=/data \
    --env "OMNIGREX_PI_UPGRADE_SOURCE_IMAGE=$source_binding" --env "OMNIGREX_PI_UPGRADE_TARGET_IMAGE=$target_binding" \
    --env "OMNIGREX_PI_UPGRADE_SOURCE_LOCAL=$source_local" --env "OMNIGREX_PI_UPGRADE_TARGET_LOCAL=$target_local" --env "OMNIGREX_PI_UPGRADE_ARCH=$arch" \
    --env "OMNIGREX_PI_UPGRADE_SOURCE_VOLUME=$source_volume" --env "OMNIGREX_PI_UPGRADE_CANDIDATE_VOLUME=$candidate_volume" --env "OMNIGREX_PI_UPGRADE_RESTORE_VOLUME=$restore_volume" \
    --env "OMNIGREX_PI_UPGRADE_SOURCE_VERSION=${OMNIGREX_PI_UPGRADE_SOURCE_VERSION:-1.0.3}" --env "OMNIGREX_PI_UPGRADE_TARGET_VERSION=${OMNIGREX_PI_UPGRADE_TARGET_VERSION:-1.0.4}" \
    --env OMNIGREX_PI_UPGRADE_ARTIFACT=/tmp/pi-upgrade-result.json \
    --env "OMNIGREX_PI_UPGRADE_RESTORE_ARCHIVE=$restore_input" \
    --env "OMNIGREX_PI_UPGRADE_SOURCE_CONTRACT_FILE=$source_contract" \
    --entrypoint /usr/local/bin/pi.test omnigrex/pi-upgrade-qualification:docker -test.v -test.run '^TestPiControlledStateUpgrade$' -test.timeout 10m >/dev/null
fi
if [ -n "${OMNIGREX_PI_UPGRADE_RESTORE_ARCHIVE:-}" ]; then
  # The replacement host receives only a private checksummed Session archive.
  cp "$OMNIGREX_PI_UPGRADE_RESTORE_ARCHIVE" "$archives/restore.tar"
  chmod 0644 "$archives/restore.tar"
  docker cp "$archives/restore.tar" "$wrapper:/tmp/pi-upgrade-input.tar"
fi
if [ -n "$source_contract" ]; then
  cp "$OMNIGREX_PI_UPGRADE_SOURCE_CONTRACT_FILE" "$archives/source-contract.json"
  chmod 0644 "$archives/source-contract.json"
  docker cp "$archives/source-contract.json" "$wrapper:$source_contract"
fi
docker start --attach "$wrapper"
test "$(docker inspect "$wrapper" --format '{{.State.ExitCode}}')" = 0
if [ -n "$artifact" ]; then
  docker cp "$wrapper:/tmp/pi-upgrade-result.json" "$archives/result.json"
  go run ./scripts/publish-qualification-output compatibility "$archives/result.json" "$artifact"
fi
if [ -n "${OMNIGREX_PI_UPGRADE_EXPORT_ARCHIVE:-}" ]; then
  docker cp "$wrapper:/tmp/pi-upgrade-source-state.tar" "$archives/state.tar"
  go run ./scripts/publish-qualification-output state "$archives/state.tar" "$OMNIGREX_PI_UPGRADE_EXPORT_ARCHIVE"
fi
