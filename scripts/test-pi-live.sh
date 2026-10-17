#!/bin/sh
set -eu
: "${OMNIGREX_PI_LIVE_AUTH_FILE:?set the local path to a Pi API-key auth file}"
auth="$OMNIGREX_PI_LIVE_AUTH_FILE"
test -f "$auth" && test -r "$auth"
backend="${OMNIGREX_PI_BACKEND:-docker}"
pattern="${OMNIGREX_PI_LIVE_TEST_PATTERN:-^TestPiLiveProviders$}"
case "$pattern" in '^TestPiLiveProviders$'|'^TestPiProviderSmokeBootstrap$') ;; *) exit 2 ;; esac
case "$backend" in
  docker) ;;
  microsandbox) : "${OMNIGREX_MICROSANDBOX_KVM_GID:?set the approved Docker host KVM group}" ;;
  *) exit 2 ;;
esac
namespace="pi-live-$(date +%s)-$$"
volume="$namespace-state"
network="$namespace-network"
wrapper="$namespace-wrapper"
image="omnigrex/pi-live:$namespace"
archives="$(mktemp -d)"
cleanup() {
  docker rm -f -v "$wrapper" >/dev/null 2>&1 || true
  docker volume rm "$volume" >/dev/null 2>&1 || true
  docker network rm "$network" >/dev/null 2>&1 || true
  docker image rm "$image" >/dev/null 2>&1 || true
  rm -rf "$archives"
}
trap cleanup EXIT HUP INT TERM
docker build --quiet --file agent/pi/Dockerfile --target golden --tag "$image" .
local_image="$(docker image inspect "$image" --format '{{.Id}}')"
arch="$(docker image inspect "$image" --format '{{.Architecture}}')"
binding="registry.example/omnigrex/pi-live@$local_image"
if [ "$backend" = microsandbox ]; then
  test "$arch" = amd64
  docker build --quiet --file Dockerfile.microsandbox --target pi-qualification --tag omnigrex/pi-live-qualification:native .
  qualification=omnigrex/pi-live-qualification:native
  set -- --device /dev/kvm --group-add "$OMNIGREX_MICROSANDBOX_KVM_GID"
else
  docker build --quiet --file Dockerfile.pi-qualification --tag omnigrex/pi-live-qualification:docker .
  qualification=omnigrex/pi-live-qualification:docker
  gid="$(docker run --rm --user 0:0 --entrypoint /usr/bin/stat --mount type=bind,src=/var/run/docker.sock,dst=/var/run/docker.sock "$image" -c %g /var/run/docker.sock)"
  set -- --group-add "$gid" --mount type=bind,src=/var/run/docker.sock,dst=/var/run/docker.sock
fi
docker network create "$network" >/dev/null
docker volume create "$volume" >/dev/null
docker run --rm --network none --user 0:0 --entrypoint /bin/sh --mount "type=volume,src=$volume,dst=/data" "$image" -ec 'chown 10001:10001 /data; chmod 0700 /data'
# The credential bundle goes only through stdin into wrapper tmpfs. The selected
# provider then reaches the production runner through its normal bootstrap.
docker create --interactive --name "$wrapper" --network "$network" "$@" \
  --memory 4g --memory-swap 4g --cpus 2 --pids-limit 2048 \
  --tmpfs /run/pi-live:rw,noexec,nosuid,nodev,size=1m,uid=10001,gid=10001,mode=0700 \
  --mount "type=volume,src=$volume,dst=/data" \
  --env HOME=/tmp --env OMNIGREX_PI_LIVE_INTEGRATION=1 --env "OMNIGREX_PI_BACKEND=$backend" \
  --env OMNIGREX_PI_LIVE_AUTH_FILE=/run/pi-live/auth.json --env OMNIGREX_PI_LIVE_ROOT=/data \
  --env "OMNIGREX_PI_LIVE_TEST_PATTERN=$pattern" \
  --env "OMNIGREX_PI_LIVE_IMAGE=$binding" --env "OMNIGREX_PI_LIVE_LOCAL_IMAGE=$local_image" \
  --env "OMNIGREX_PI_LIVE_ARCH=$arch" --env "OMNIGREX_PI_LIVE_VOLUME=$volume" --env "OMNIGREX_PI_LIVE_NETWORK=$network" \
  --env "OMNIGREX_PI_OPENAI_MODEL=${OMNIGREX_PI_OPENAI_MODEL:-o4-mini}" \
  --env "OMNIGREX_PI_OPENCODE_MODEL=${OMNIGREX_PI_OPENCODE_MODEL:-muse-spark-1.3-contributor}" \
  --entrypoint /bin/sh "$qualification" \
  -ec 'umask 077; cat > /run/pi-live/auth.json; exec /usr/local/bin/pi.test -test.v -test.run "$OMNIGREX_PI_LIVE_TEST_PATTERN" -test.timeout 10m' >/dev/null
if [ "$backend" = microsandbox ]; then
  docker image save --output "$archives/image.tar" "$image"
  chmod 0644 "$archives/image.tar"
  docker cp "$archives/image.tar" "$wrapper:/tmp/pi-live-image.tar"
fi
docker start --attach --interactive "$wrapper" < "$auth"
test "$(docker inspect "$wrapper" --format '{{.State.ExitCode}}')" = 0
