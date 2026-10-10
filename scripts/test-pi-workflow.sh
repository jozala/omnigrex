#!/bin/sh
set -eu
backend="${OMNIGREX_PI_BACKEND:-docker}"
pattern="${OMNIGREX_PI_WORKFLOW_TEST_PATTERN:-^TestPiControlledWorkflow$}"
namespace="pi-workflow-$(date +%s)-$$"
network="$namespace-network"
database="$namespace-database"
workspaces="$namespace-workspaces"
states="$namespace-states"
tools="$namespace-tools"
publication="$namespace-publication"
cleanup() {
  docker rm -f "$database" >/dev/null 2>&1 || true
  docker volume rm "$workspaces" "$states" "$tools" "$publication" >/dev/null 2>&1 || true
  docker network rm "$network" >/dev/null 2>&1 || true
}
trap cleanup EXIT HUP INT TERM
docker build --quiet --file agent/pi/Dockerfile --target workflow-fixture --tag omnigrex/pi-workflow:step12 .
if [ "$backend" = microsandbox ]; then
  : "${OMNIGREX_MICROSANDBOX_KVM_GID:?set the approved Docker host KVM group}"
  docker build --quiet --file Dockerfile.microsandbox --target pi-workflow-qualification --tag omnigrex/pi-workflow-qualification:step12 .
else
  docker build --quiet --file Dockerfile.pi-qualification --tag omnigrex/pi-workflow-qualification:step12 .
fi
docker network create "$network" >/dev/null
docker run --detach --rm --name "$database" --network "$network" --env POSTGRES_PASSWORD=qualification-only-password --env POSTGRES_DB=qualification postgres:18-alpine@sha256:77f585114c32fbca283dc835b0596f4e52b51b4c6662d7810b2f4084f60a1873 >/dev/null
for volume in "$workspaces" "$states" "$tools" "$publication"; do docker volume create "$volume" >/dev/null; done
docker run --rm --user 0:0 --entrypoint /bin/sh --mount "type=volume,src=$workspaces,dst=/data/workspaces" --mount "type=volume,src=$states,dst=/data/state" --mount "type=volume,src=$tools,dst=/data/tools" --mount "type=volume,src=$publication,dst=/data/publication" omnigrex/pi-workflow:step12 -ec 'chown 10001:10001 /data/workspaces /data/state /data/tools /data/publication'
attempt=0
until docker exec "$database" pg_isready -U postgres -d qualification >/dev/null 2>&1; do
  attempt=$((attempt+1)); test "$attempt" -lt 100; sleep .1
done
if [ "$backend" = docker ]; then
  local_image="$(docker image inspect omnigrex/pi-workflow:step12 --format '{{.Id}}')"
  image="registry.example/omnigrex/pi-workflow@$local_image"
  arch="$(docker image inspect omnigrex/pi-workflow:step12 --format '{{.Architecture}}')"
  gid="$(docker run --rm --user 0:0 --entrypoint /usr/bin/stat --mount type=bind,src=/var/run/docker.sock,dst=/var/run/docker.sock omnigrex/pi-workflow:step12 -c %g /var/run/docker.sock)"
  docker run --rm --network "$network" --group-add "$gid" --mount type=bind,src=/var/run/docker.sock,dst=/var/run/docker.sock \
    --mount "type=volume,src=$workspaces,dst=/data/workspaces" --mount "type=volume,src=$states,dst=/data/state" --mount "type=volume,src=$tools,dst=/data/tools" --mount "type=volume,src=$publication,dst=/data/publication" \
    --env OMNIGREX_PI_WORKFLOW_INTEGRATION=1 --env OMNIGREX_PI_BACKEND=docker --env OMNIGREX_PI_WORKFLOW_ROOT=/data \
    --env "OMNIGREX_PI_WORKFLOW_DATABASE_URL=postgres://postgres@$database:5432/qualification?sslmode=disable" \
    --env "OMNIGREX_PI_WORKFLOW_NETWORK=$network" --env "OMNIGREX_PI_WORKFLOW_IMAGE=$image" --env "OMNIGREX_PI_WORKFLOW_ARCH=$arch" \
    --env "OMNIGREX_PI_WORKFLOW_LOCAL_IMAGE=$local_image" \
    --env "OMNIGREX_PI_WORKFLOW_WORKSPACES=$workspaces" --env "OMNIGREX_PI_WORKFLOW_STATES=$states" --env "OMNIGREX_PI_WORKFLOW_TOOLS=$tools" \
    omnigrex/pi-workflow-qualification:step12 -test.v -test.run "$pattern" -test.timeout 30m
else
  docker image save omnigrex/pi-workflow:step12 | docker run --rm -i --network "$network" --device /dev/kvm --group-add "$OMNIGREX_MICROSANDBOX_KVM_GID" \
    --memory 6g --memory-swap 6g --cpus 2 --pids-limit 2048 \
    --mount "type=volume,src=$workspaces,dst=/data/workspaces" --mount "type=volume,src=$states,dst=/data/state" --mount "type=volume,src=$tools,dst=/data/tools" --mount "type=volume,src=$publication,dst=/data/publication" \
    --env OMNIGREX_PI_WORKFLOW_INTEGRATION=1 --env OMNIGREX_PI_BACKEND=microsandbox --env OMNIGREX_PI_WORKFLOW_ROOT=/data \
    --env "OMNIGREX_PI_WORKFLOW_DATABASE_URL=postgres://postgres@$database:5432/qualification?sslmode=disable" \
    --env "OMNIGREX_PI_WORKFLOW_TEST_PATTERN=$pattern" \
    --entrypoint /bin/sh omnigrex/pi-workflow-qualification:step12 -ec 'cat > /tmp/pi-workflow-image.tar; exec /usr/local/bin/workflow.test -test.v -test.run "$OMNIGREX_PI_WORKFLOW_TEST_PATTERN" -test.timeout 30m'
fi
