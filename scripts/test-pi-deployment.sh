#!/bin/sh
# Run on the Docker daemon's host: Compose secrets are host-side bind mounts.
set -eu
backend="${OMNIGREX_PI_BACKEND:-docker}"
case "$backend" in docker|microsandbox) ;; *) exit 2 ;; esac
rehearsal="${OMNIGREX_PI_CUTOVER_REHEARSAL:-0}"
case "$rehearsal" in 0|1) ;; *) exit 2 ;; esac
source_root="$PWD"
if [ "$rehearsal" = 1 ]; then
  . "$source_root/scripts/pi-cutover-rehearsal.sh"
fi
namespace="pi-deploy-$(date +%s)-$$"
directory="$(mktemp -d)"
registry="$namespace-registry"
registry_started=0
COMPOSE_FILE="compose.pi.yaml:compose.pi-$backend.yaml:smoke.yaml"
# Deployment smoke must never inherit an operator's App IDs, secret paths,
# image selection, or stable Compose project from the invoking shell.
compose() {
  env -i PATH="$PATH" HOME="$HOME" \
    DOCKER_HOST="${DOCKER_HOST:-}" DOCKER_CONTEXT="${DOCKER_CONTEXT:-}" \
    DOCKER_CONFIG="${DOCKER_CONFIG:-$HOME/.docker}" \
    COMPOSE_FILE="$COMPOSE_FILE" COMPOSE_PROJECT_NAME="$namespace" docker compose "$@"
}
ensure_fixture_registry() {
  if [ "$registry_started" = 1 ]; then return; fi
  # Bind in the daemon's network namespace, including Docker Desktop's VM.
  # Both source and target fixture publication share this one registry.
  registry_port="$((30000 + $$ % 20000))"
  docker run --detach --rm --name "$registry" --network host \
    --env "REGISTRY_HTTP_ADDR=127.0.0.1:$registry_port" registry:2@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373 >/dev/null
  registry_started=1
  attempt=0
  until docker exec "$registry" wget -q -O /dev/null "http://127.0.0.1:$registry_port/v2/"; do
    attempt=$((attempt+1)); test "$attempt" -lt 30; sleep 1
  done
}
cleanup() {
  status=$?
  if [ -f "$directory/.env" ]; then
    if [ "$status" -ne 0 ]; then
      (cd "$directory" && compose logs --no-color orchestrator) >&2 || true
    fi
    if ! (cd "$directory" && compose down --volumes --remove-orphans --timeout 10) >/dev/null 2>&1; then
      printf 'Failed to remove disposable deployment resources: %s\n' "$namespace" >&2
      status=1
    fi
  fi
  docker rm -f "$registry" >/dev/null 2>&1 || true
  rm -rf "$directory"
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' HUP TERM
orchestrator="${OMNIGREX_PI_DEPLOY_ORCHESTRATOR_IMAGE:-}"
pi="${OMNIGREX_PI_DEPLOY_IMAGE:-}"
if [ -z "$orchestrator" ] && [ -z "$pi" ]; then
  docker build --quiet --file agent/pi/Dockerfile --target golden --tag omnigrex/pi:1.0.4 .
  if [ "$backend" = docker ]; then
    docker build --quiet --tag omnigrex/orchestrator:dev .
    ensure_fixture_registry
    for image in pi:1.0.4 orchestrator:dev; do
      name="${image%%:*}"
      docker tag "omnigrex/$image" "127.0.0.1:$registry_port/omnigrex/$name:fixture"
      docker push "127.0.0.1:$registry_port/omnigrex/$name:fixture" >/dev/null
    done
    orchestrator="$(docker image inspect "127.0.0.1:$registry_port/omnigrex/orchestrator:fixture" --format '{{index .RepoDigests 0}}')"
    pi="$(docker image inspect "127.0.0.1:$registry_port/omnigrex/pi:fixture" --format '{{index .RepoDigests 0}}')"
  else
    : "${OMNIGREX_MICROSANDBOX_KVM_GID:?set the host KVM group}"
    docker build --quiet --file Dockerfile.microsandbox --target runtime --tag omnigrex/orchestrator-microsandbox:dev .
    orchestrator=omnigrex/orchestrator-microsandbox:dev
    pi=omnigrex/pi:1.0.4
  fi
elif [ -z "$orchestrator" ] || [ -z "$pi" ]; then
  printf '%s\n' 'Supply both published image digests or neither.' >&2
  exit 2
else
  case "$orchestrator,$pi" in *@sha256:*,*@sha256:*) ;; *) exit 2 ;; esac
  docker pull --platform linux/amd64 "$orchestrator"
  docker pull --platform linux/amd64 "$pi"
fi
arch="$(docker image inspect "$pi" --format '{{.Architecture}}')"
if [ "$rehearsal" = 1 ]; then
  prepare_legacy_images
fi
cp deploy/compose.pi.yaml deploy/compose.pi-"$backend".yaml "$directory/"
mkdir "$directory/secrets"
openssl rand -hex 32 > "$directory/secrets/database-password"
openssl rand -hex 32 > "$directory/secrets/github-webhook-secret"
openssl genrsa -out "$directory/secrets/github-developer-private-key.pem" 2048 2>/dev/null
openssl genrsa -out "$directory/secrets/github-reviewer-private-key.pem" 2048 2>/dev/null
printf '%s\n' '{"openai":{"type":"api_key","key":"smoke-fixture-only"}}' > "$directory/secrets/provider-credentials.json"
chmod 0640 "$directory"/secrets/*
cat > "$directory/.env" <<EOF
COMPOSE_PROJECT_NAME=$namespace
COMPOSE_FILE=compose.pi.yaml:compose.pi-$backend.yaml:smoke.yaml
OMNIGREX_ORCHESTRATOR_IMAGE=$orchestrator
OMNIGREX_PI_RPC_V1_IMAGE=$pi
OMNIGREX_PI_RPC_V1_PLATFORM=linux/$arch
OMNIGREX_SECRET_GID=$(id -g)
OMNIGREX_DOCKER_GID=$(stat -c '%g' /var/run/docker.sock 2>/dev/null || printf 0)
OMNIGREX_MICROSANDBOX_KVM_GID=${OMNIGREX_MICROSANDBOX_KVM_GID:-0}
OMNIGREX_GITHUB_DEVELOPER_APP_ID=1
OMNIGREX_GITHUB_REVIEWER_APP_ID=2
OMNIGREX_DATABASE_PASSWORD_FILE=./secrets/database-password
OMNIGREX_GITHUB_DEVELOPER_PRIVATE_KEY_FILE=./secrets/github-developer-private-key.pem
OMNIGREX_GITHUB_REVIEWER_PRIVATE_KEY_FILE=./secrets/github-reviewer-private-key.pem
OMNIGREX_GITHUB_WEBHOOK_SECRET_FILE=./secrets/github-webhook-secret
OMNIGREX_PROVIDER_CREDENTIALS_FILE=./secrets/provider-credentials.json
OMNIGREX_HTTP_PORT=0
EOF
cat > "$directory/smoke.yaml" <<EOF
networks:
  backend:
    name: $namespace-backend
  agent:
    name: $namespace-agent
volumes:
  postgres-data:
    name: $namespace-postgres
  workspaces:
    name: $namespace-workspaces
  runtime-state:
    name: $namespace-runtime-state
  mise-data:
    name: $namespace-mise
services:
  orchestrator:
    environment:
      OMNIGREX_DOCKER_AGENT_NETWORK: $namespace-agent
      OMNIGREX_WORKSPACE_VOLUME: $namespace-workspaces
      OMNIGREX_RUNTIME_STATE_VOLUME: $namespace-runtime-state
      OMNIGREX_MISE_VOLUME: $namespace-mise
      OMNIGREX_MICROSANDBOX_NAMESPACE: $namespace
EOF
cd "$directory"
if [ "$rehearsal" = 1 ]; then
  rehearse_legacy_reset
fi
if [ "$backend" = microsandbox ]; then
  cat > native-smoke.yaml <<EOF
volumes:
  microsandbox-home:
    name: $namespace-msb
EOF
  COMPOSE_FILE="$COMPOSE_FILE:native-smoke.yaml"
  if [ "$pi" = omnigrex/pi:1.0.4 ]; then
    # Docker archive import synthesizes a new manifest. Only source fixtures
    # may bind that imported digest; it is not registry-image release evidence.
    docker image save "$pi" > pi.tar
    compose run --rm --no-deps -T --entrypoint /usr/local/bin/msb orchestrator image load --tag "$pi" < pi.tar
    inspect="$(compose run --rm --no-deps -T --entrypoint /usr/local/bin/msb orchestrator image inspect "$pi" --format json)"
    digest="$(printf '%s' "$inspect" | jq -er '.digest')"
    image="registry.example/omnigrex/pi@$digest"
    compose run --rm --no-deps -T --entrypoint /usr/local/bin/msb orchestrator image load --tag "$image" < pi.tar
    cat >> native-smoke.yaml <<EOF
services:
  orchestrator:
    environment:
      OMNIGREX_PI_RPC_V1_IMAGE: $image
EOF
    rm pi.tar
  else
    # Preserve the published registry manifest/index and its platform linkage
    # in this fresh native cache, matching the real deployment procedure.
    compose run --rm --no-deps -T --entrypoint /usr/local/bin/msb orchestrator image pull "$pi"
  fi
fi
compose up --no-build --wait --wait-timeout 180
port="$(compose port orchestrator 8080 | cut -d : -f 2)"
curl -fsS "http://127.0.0.1:$port/healthz"
curl -fsS "http://127.0.0.1:$port/readyz"
# External GitHub fixture IDs intentionally fail. All independent operational
# checks must pass, including disposable Pi/MCP initialization without prompts.
compose exec -T orchestrator /usr/local/bin/omnigrex doctor --repository fixture/repository > doctor.txt 2>&1 || true
cat doctor.txt
for check in runtime-profile-contract runtime-profile-image pi-runtime postgres-connectivity postgres-schema protected-runtime-profiles pending-runtime-profile-compatibility mcp-endpoint; do
  grep -q "^PASS $check$" doctor.txt
done
if [ "$backend" = docker ]; then
  grep -q '^PASS docker-resources$' doctor.txt
else
  grep -q '^PASS microsandbox-kvm-resources$' doctor.txt
fi
compose exec -T orchestrator /bin/sh -ec 'files="$(find /var/lib/omnigrex/runtime-state -type f -print)"; test -z "$files"'
if [ "$rehearsal" = 1 ]; then
  rehearse_pi_recovery
fi
if [ "$backend" = microsandbox ]; then
  cat > missing-kvm.yaml <<EOF
services:
  orchestrator:
    devices: !reset []
EOF
  if COMPOSE_FILE="$COMPOSE_FILE:missing-kvm.yaml" compose run --rm --no-deps -T orchestrator > missing-kvm.txt 2>&1; then
    printf '%s\n' 'Native startup accepted work without KVM.' >&2
    exit 1
  fi
  grep -q 'Microsandbox requires usable /dev/kvm access' missing-kvm.txt
fi
printf 'PASS image-only Pi deployment and disposable diagnostics (%s)\n' "$backend"
