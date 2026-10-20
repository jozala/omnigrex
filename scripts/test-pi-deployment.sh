#!/bin/sh
# Run on the Docker daemon's host: Compose secrets are host-side bind mounts.
set -eu
backend="${OMNIGREX_PI_BACKEND:-docker}"
case "$backend" in docker|microsandbox) ;; *) exit 2 ;; esac
recovery="${OMNIGREX_PI_DEPLOY_RECOVERY:-1}"
case "$recovery" in 0|1) ;; *) exit 2 ;; esac
. "$PWD/scripts/pi-recovery-smoke.sh"
namespace="pi-deploy-$(date +%s)-$$"
directory="$(mktemp -d)"
registry="$namespace-registry"
registry_started=0
COMPOSE_FILE="compose.pi.yaml:compose.pi-$backend.yaml:smoke.yaml"
# Cleanup is authorized for an exact isolated file set, not ambient selection.
cleanup_compose_file=
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
  # Source-built Pi targets publish through this disposable registry.
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
  if [ -n "$cleanup_compose_file" ] && [ -f "$directory/.env" ]; then
    if [ "$status" -ne 0 ]; then
      (cd "$directory" && COMPOSE_FILE="$cleanup_compose_file" compose logs --no-color orchestrator) >&2 || true
    fi
    if ! (cd "$directory" && COMPOSE_FILE="$cleanup_compose_file" compose down --volumes --remove-orphans --timeout 10) >/dev/null 2>&1; then
      printf 'Failed to remove disposable deployment resources: %s\n' "$namespace" >&2
      status=1
    fi
  fi
  docker rm -f -v "$registry" >/dev/null 2>&1 || true
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
cp deploy/compose.pi.yaml deploy/compose.pi-"$backend".yaml "$directory/"
mkdir "$directory/secrets"
openssl rand -hex 32 > "$directory/secrets/database-password"
openssl rand -hex 32 > "$directory/secrets/github-webhook-secret"
openssl genrsa -out "$directory/secrets/github-developer-private-key.pem" 2048 2>/dev/null
openssl genrsa -out "$directory/secrets/github-reviewer-private-key.pem" 2048 2>/dev/null
printf '%s\n' '{"openai":{"type":"api_key","key":"smoke-fixture-only"}}' > "$directory/secrets/provider-credentials.json"
chmod 0640 "$directory"/secrets/*
docker_gid="$(stat -c '%g' /var/run/docker.sock 2>/dev/null || printf 0)"
kvm_gid="${OMNIGREX_MICROSANDBOX_KVM_GID:-0}"
# Keep fixture secrets separate from either backend's host authority, even
# when CI assigns /dev/kvm to the invoking user's group.
secret_gid=10001
while [ "$secret_gid" = "$docker_gid" ] || [ "$secret_gid" = "$kvm_gid" ]; do
  secret_gid=$((secret_gid + 1))
done
docker run --rm --network none --user 0:0 --entrypoint /bin/sh \
  --mount "type=bind,src=$directory/secrets,dst=/secrets" "$pi" \
  -ec 'chgrp "$1" /secrets/*' sh "$secret_gid"
cat > "$directory/.env" <<EOF
COMPOSE_PROJECT_NAME=$namespace
COMPOSE_FILE=compose.pi.yaml:compose.pi-$backend.yaml:smoke.yaml
OMNIGREX_ORCHESTRATOR_IMAGE=$orchestrator
OMNIGREX_PI_IMAGE=$pi
OMNIGREX_PI_PLATFORM=linux/$arch
OMNIGREX_SECRET_GID=$secret_gid
OMNIGREX_DOCKER_GID=$docker_gid
OMNIGREX_MICROSANDBOX_KVM_GID=$kvm_gid
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
  provider-auth:
    name: $namespace-provider-auth
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
# Compose canonicalizes secret paths, including macOS's /var -> /private/var.
directory="$(pwd -P)"
if [ "$backend" = microsandbox ]; then
  cat > native-smoke.yaml <<EOF
volumes:
  microsandbox-home:
    name: $namespace-msb
EOF
  COMPOSE_FILE="$COMPOSE_FILE:native-smoke.yaml"
  cleanup_compose_file="$COMPOSE_FILE"
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
      OMNIGREX_PI_IMAGE: $image
EOF
    rm pi.tar
  else
    # Preserve the published registry manifest/index and its platform linkage
    # in this fresh native cache, matching the real deployment procedure.
    compose run --rm --no-deps -T --entrypoint /usr/local/bin/msb orchestrator image pull "$pi"
  fi
fi
cleanup_compose_file="$COMPOSE_FILE"
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
# Preserve the full-stack regression gates in this disposable Pi topology.
psql() {
  compose exec -T postgres psql --no-psqlrc --quiet --tuples-only --no-align \
    --set ON_ERROR_STOP=1 --username omnigrex --dbname omnigrex --command "$1"
}
await_sql() {
  attempt=0
  until [ "$(psql "$1")" = "$2" ]; do
    attempt=$((attempt+1)); test "$attempt" -lt 60; sleep 1
  done
}
send_webhook() {
  signature="$(printf '%s' "$4" | openssl dgst -sha256 -hmac "$3")"
  curl -sS -o /dev/null -w '%{http_code}' \
    -H 'Content-Type: application/json' -H "X-GitHub-Delivery: $1" \
    -H "X-GitHub-Event: $2" -H "X-Hub-Signature-256: sha256=${signature##* }" \
    --data-binary "$4" "http://127.0.0.1:$port/webhooks/github"
}
webhook_secret="$(cat secrets/github-webhook-secret)"
delivery=123e4567-e89b-42d3-a456-426614174000
body='{"action":"closed","repository":{"id":9123,"name":"fixture","owner":{"login":"fixture"}},"issue":{"id":456,"number":12}}'
test "$(send_webhook "$delivery" issues "$webhook_secret" "$body")" = 202
test "$(send_webhook "$delivery" issues "$webhook_secret" "$body")" = 202
await_sql "SELECT delivery.status || ':' || count(event.delivery_id)::text FROM webhook_deliveries AS delivery LEFT JOIN normalized_events AS event ON event.delivery_id = delivery.delivery_id WHERE delivery.delivery_id = '$delivery' GROUP BY delivery.status" PROCESSED:1
ignored=323e4567-e89b-42d3-a456-426614174000
test "$(send_webhook "$ignored" ping "$webhook_secret" 'not JSON')" = 202
await_sql "SELECT status FROM webhook_deliveries WHERE delivery_id = '$ignored'" IGNORED
test "$(psql "SELECT count(*) FROM normalized_events WHERE delivery_id = '$ignored'")" = 0
invalid=223e4567-e89b-42d3-a456-426614174000
test "$(send_webhook "$invalid" issues wrong-secret "$body")" = 401
test "$(psql "SELECT count(*) FROM webhook_deliveries WHERE delivery_id = '$invalid'")" = 0
unset webhook_secret
compose exec -T orchestrator /bin/sh -ec '
  for root in runtime-state workspaces mise; do
    printf persistent > "/var/lib/omnigrex/$root/.pi-deployment-marker"
  done'
compose up -d --force-recreate --no-deps --wait --wait-timeout 180 orchestrator
port="$(compose port orchestrator 8080 | cut -d : -f 2)"
compose exec -T orchestrator /bin/sh -ec '
  for root in runtime-state workspaces mise; do
    test "$(cat "/var/lib/omnigrex/$root/.pi-deployment-marker")" = persistent
    rm "/var/lib/omnigrex/$root/.pi-deployment-marker"
  done'
psql "CREATE TABLE deployment_marker (value text); INSERT INTO deployment_marker VALUES ('persistent-ledger')"
compose up -d --force-recreate --no-deps --wait --wait-timeout 180 postgres
test "$(psql 'SELECT value FROM deployment_marker')" = persistent-ledger
test "$(psql "SELECT status FROM webhook_deliveries WHERE delivery_id = '$delivery'")" = PROCESSED
postgres_id="$(compose ps --quiet postgres)"
postgres_ip="$(docker inspect --format "{{(index .NetworkSettings.Networks \"$namespace-backend\").IPAddress}}" "$postgres_id")"
test -n "$postgres_ip"
docker run --rm --network "$namespace-agent" --user 10001:10001 --read-only \
  --cap-drop ALL --security-opt no-new-privileges --entrypoint node "$pi" -e '
    const socket = require("net").connect(5432, process.argv[1]);
    socket.on("connect", () => process.exit(1));
    socket.on("error", () => process.exit(0));
    socket.setTimeout(2000, () => { socket.destroy(); process.exit(0); });
  ' "$postgres_ip"
compose stop --timeout 10 postgres
test "$(curl -sS -o /dev/null -w '%{http_code}' "http://127.0.0.1:$port/healthz")" = 200
test "$(curl --max-time 70 -sS -o /dev/null -w '%{http_code}' "http://127.0.0.1:$port/readyz")" = 503
test "$(curl -sS -o /dev/null -w '%{http_code}' "http://127.0.0.1:$port/healthz")" = 200
compose start postgres
compose up --no-build --wait --wait-timeout 180
curl -fsS "http://127.0.0.1:$port/readyz"
printf 'PASS durable authenticated webhooks, persistent storage, network isolation and readiness (%s)\n' "$backend"
if [ "$recovery" = 1 ]; then
  verify_pi_recovery
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
