#!/bin/sh
set -eu

# This script runs only inside the disposable microVM, never on the Docker host.
test ! -S /var/run/docker.sock
unset DOCKER_CONTEXT DOCKER_TLS_VERIFY DOCKER_CERT_PATH
export DOCKER_HOST=unix:///var/run/docker.sock
export DOCKER_TLS_CERTDIR=

uname -srmo
id
if test -c /dev/kvm; then
    printf 'Nested KVM device present; nested microVM execution is not qualified by this probe.\n'
else
    printf 'Nested KVM device absent; run Microsandbox integration tests on the outer KVM host.\n'
fi

dockerd --host "$DOCKER_HOST" >/tmp/dockerd.log 2>&1 &
daemon=$!
cleanup() {
    docker compose -p omnigrex-feasibility -f /probe/compose.yaml logs --no-color || true
    docker compose -p omnigrex-feasibility -f /probe/compose.yaml down --volumes || true
    kill "$daemon" 2>/dev/null || true
    wait "$daemon" 2>/dev/null || true
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

attempt=0
until docker info >/dev/null 2>&1; do
    attempt=$((attempt + 1))
    if test "$attempt" -ge 60 || ! kill -0 "$daemon" 2>/dev/null; then
        cat /tmp/dockerd.log
        exit 1
    fi
    sleep 1
done
docker version
docker compose version
docker info --format 'Nested engine: {{.ServerVersion}}; storage={{.Driver}}; CPUs={{.NCPU}}; memory={{.MemTotal}}'
test "$(docker info --format '{{.NCPU}}')" -eq 2
memory=$(docker info --format '{{.MemTotal}}')
test "$memory" -gt 3221225472
test "$memory" -le 4294967296
test -z "$(docker ps -aq)"

docker compose -p omnigrex-feasibility -f /probe/compose.yaml up --wait --wait-timeout 60 web
docker compose -p omnigrex-feasibility -f /probe/compose.yaml run --rm --no-deps client
docker compose -p omnigrex-feasibility -f /probe/compose.yaml down --volumes
printf 'PASS: nested Docker Compose service networking and health checks\n'

# Package versions are emitted as evidence; this is an exploratory fixture, not a release image.
apk add --no-cache curl git gcc musl-dev
apk info -v
curl --fail --silent --show-error --location --retry 2 https://go.dev/dl/go1.27.0.linux-amd64.tar.gz -o /tmp/go.tar.gz
printf '%s  /tmp/go.tar.gz\n' 675c26c449cbb18fc24b74650de1eabbae6e16f64326fd85a283fb3b58280685 | sha256sum -c -
tar -C /usr/local -xzf /tmp/go.tar.gz
rm /tmp/go.tar.gz
export PATH=/usr/local/go/bin:$PATH
export GOTOOLCHAIN=local
export GOMAXPROCS=2
go version

cd /probe/testcontainers
# Testcontainers 0.44.0 selects this Ryuk tag; seed it from an exact digest.
docker pull docker.io/testcontainers/ryuk@sha256:7c1a8a9a47c780ed0f983770a662f80deb115d95cce3e2daa3d12115b8cd28f0
docker tag docker.io/testcontainers/ryuk@sha256:7c1a8a9a47c780ed0f983770a662f80deb115d95cce3e2daa3d12115b8cd28f0 testcontainers/ryuk:0.14.0
cp fixture.mod go.mod
cp fixture.sum go.sum
go test -count=1 -timeout 5m -v .
printf 'PASS: Testcontainers published-port HTTP request\n'

mkdir /workspace
tar -xf /probe/source.tar -C /workspace
cd /workspace
docker build --file agent/opencode/Dockerfile --tag omnigrex/opencode:1.18.29 .
if ! go test -p 1 -race -tags=integration -timeout 10m -count=1 -v ./internal/runtime/docker -run '^TestReadinessProbeExercisesRealDockerResources$' >/tmp/readiness-test.log 2>&1; then
    cat /tmp/readiness-test.log
    exit 1
fi
cat /tmp/readiness-test.log
# A renamed or removed test must not turn this gate into a successful empty run.
grep -q '^--- PASS: TestReadinessProbeExercisesRealDockerResources ' /tmp/readiness-test.log
printf 'PASS: Omnigrex Docker resource readiness integration inside the microVM\n'
