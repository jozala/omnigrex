# Sourced only by the isolated deployment smoke, never by an operator deployment.
# Every destructive operation uses the smoke's generated project and volume names.
prepare_legacy_images() {
  legacy_orchestrator="${OMNIGREX_PI_CUTOVER_SOURCE_ORCHESTRATOR_IMAGE:-}"
  legacy_agent="${OMNIGREX_PI_CUTOVER_SOURCE_AGENT_IMAGE:-}"
  if [ -z "$legacy_orchestrator" ] && [ -z "$legacy_agent" ]; then
    docker build --quiet --platform "linux/$arch" --tag omnigrex/orchestrator:cutover-source .
    docker build --quiet --platform "linux/$arch" --file agent/opencode/Dockerfile --tag omnigrex/opencode:cutover-source .
    ensure_fixture_registry
    for name in orchestrator opencode; do
      docker tag "omnigrex/$name:cutover-source" "127.0.0.1:$registry_port/omnigrex/$name:cutover-source"
      docker push "127.0.0.1:$registry_port/omnigrex/$name:cutover-source" >/dev/null
    done
    legacy_orchestrator="$(docker image inspect "127.0.0.1:$registry_port/omnigrex/orchestrator:cutover-source" --format '{{index .RepoDigests 0}}')"
    legacy_agent="$(docker image inspect "127.0.0.1:$registry_port/omnigrex/opencode:cutover-source" --format '{{index .RepoDigests 0}}')"
  elif [ -z "$legacy_orchestrator" ] || [ -z "$legacy_agent" ]; then
    printf '%s\n' 'Supply both cutover source registry digests or neither.' >&2
    exit 2
  else
    case "$legacy_orchestrator,$legacy_agent" in *@sha256:*,*@sha256:*) ;; *) exit 2 ;; esac
    docker pull --platform "linux/$arch" "$legacy_orchestrator"
    docker pull --platform "linux/$arch" "$legacy_agent"
  fi
}

rehearse_legacy_reset() {
  pi_compose="$COMPOSE_FILE"
  cp "$source_root/deploy/compose.yaml" compose.legacy.yaml
  cat >> .env <<EOF
OMNIGREX_OPENCODE_ACP_V1_IMAGE=$legacy_agent
OMNIGREX_OPENCODE_ACP_V1_PLATFORM=linux/$arch
EOF
  cat > legacy.yaml <<EOF
services:
  orchestrator:
    image: $legacy_orchestrator
    environment:
      OMNIGREX_PI_RPC_V1_IMAGE: ""
      OMNIGREX_PI_RPC_V1_PLATFORM: ""
EOF
  printf '%s\n' '{"openai":{"type":"api","key":"cutover-fixture-only"}}' > secrets/provider-credentials.json
  COMPOSE_FILE=compose.legacy.yaml:smoke.yaml:legacy.yaml
  compose up --no-build --wait --wait-timeout 180
  # Seed unmistakable old operational data and storage in the isolated stack.
  # This is disposal evidence, not an OpenCode conversation migration fixture.
  compose exec -T postgres psql -U omnigrex -d omnigrex -v ON_ERROR_STOP=1 \
    -c 'CREATE TABLE cutover_legacy_marker (value text); INSERT INTO cutover_legacy_marker VALUES ($$old-ledger$$)'
  compose exec -T orchestrator /bin/sh -ec '
    for root in runtime-state workspaces mise; do
      printf old-state > "/var/lib/omnigrex/$root/cutover-legacy-marker"
    done'
  compose stop orchestrator
  remaining="$(docker ps --quiet --filter label=io.omnigrex.runtime-process=true --filter "volume=$namespace-runtime-state")"
  test -z "$remaining"
  # Preserve a validated pre-reset recovery set before removing disposable data.
  mkdir -m 0700 legacy-backup
  compose exec -T postgres pg_dump -U omnigrex -d omnigrex -Fc --no-owner --no-acl > legacy-backup/ledger.dump
  compose exec -T postgres pg_restore --list < legacy-backup/ledger.dump > /dev/null
  archive_volume "$namespace-runtime-state" > legacy-backup/state.tar.gz
  tar -tzf legacy-backup/state.tar.gz > /dev/null
  compose down --volumes --remove-orphans --timeout 60
  for volume in postgres runtime-state workspaces mise; do
    remaining="$(docker volume ls --filter "name=$namespace-$volume" --format '{{.Name}}')"
    if [ -n "$remaining" ]; then
      printf 'Legacy volume survived explicit reset: %s\n' "$volume" >&2
      exit 1
    fi
  done
  COMPOSE_FILE="$pi_compose"
  printf '%s\n' '{"openai":{"type":"api_key","key":"smoke-fixture-only"}}' > secrets/provider-credentials.json
  printf 'PASS isolated OpenCode backup and explicit reset (%s)\n' "$backend"
}

archive_volume() {
  docker run --rm --network none --read-only --user 0:0 --entrypoint /bin/sh \
    --mount "type=volume,src=$1,dst=/source,readonly" "$pi" -ec 'cd /source; tar -czf - .'
}

restore_volume() {
  docker run --rm -i --network none --read-only --user 0:0 --entrypoint /bin/sh \
    --mount "type=volume,src=$1,dst=/target" "$pi" \
    -ec 'find /target -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +; tar -xzf - -C /target'
}

rehearse_pi_recovery() {
  # The freshly composed installation must have no old ledger or storage.
  test "$(compose exec -T postgres psql -U omnigrex -d omnigrex -Atc "SELECT to_regclass('cutover_legacy_marker') IS NULL")" = t
  compose exec -T orchestrator /bin/sh -ec '
    for root in runtime-state workspaces mise; do
      test ! -e "/var/lib/omnigrex/$root/cutover-legacy-marker"
    done'
  compose stop orchestrator
  remaining="$(docker ps --quiet --filter label=io.omnigrex.runtime-process=true --filter "volume=$namespace-runtime-state")"
  test -z "$remaining"
  if [ "$backend" = microsandbox ]; then
    remaining="$(compose run --rm --no-deps -T --entrypoint /usr/local/bin/msb orchestrator list --quiet)"
    test -z "$remaining"
  fi
  # Marker fixtures test the coordinated procedure and numeric ownership.
  # Real ownership-bound Pi continuation/replay is qualified by the runtime gates.
  compose exec -T postgres psql -U omnigrex -d omnigrex -v ON_ERROR_STOP=1 \
    -c 'CREATE TABLE cutover_pi_marker (value text); INSERT INTO cutover_pi_marker VALUES ($$fresh-pi-ledger$$)'
  docker run --rm --network none --read-only --user 10001:10001 --entrypoint /bin/sh \
    --mount "type=volume,src=$namespace-runtime-state,dst=/state" "$pi" -ec '
      mkdir -p /state/cutover-fixture/sessions /state/workspace-captures/cutover-fixture
      printf session-fixture > /state/cutover-fixture/sessions/marker
      printf capture-fixture > /state/workspace-captures/cutover-fixture/marker
      chmod 0600 /state/cutover-fixture/sessions/marker /state/workspace-captures/cutover-fixture/marker'
  mkdir -m 0700 pi-backup
  compose exec -T postgres pg_dump -U omnigrex -d omnigrex -Fc --no-owner --no-acl > pi-backup/ledger.dump
  compose exec -T postgres pg_restore --list < pi-backup/ledger.dump > /dev/null
  archive_volume "$namespace-runtime-state" > pi-backup/state.tar.gz
  tar -tzf pi-backup/state.tar.gz > /dev/null
  if [ "$backend" = microsandbox ]; then
    # Diagnostic-only installations have not created a Turn dispatch journal.
    # Seed a non-record marker to exercise inclusion and exact restoration.
    docker run --rm --network none --read-only --user 10001:10001 --entrypoint /bin/sh \
      --mount "type=volume,src=$namespace-msb,dst=/native" "$pi" \
      -ec 'mkdir -p /native/omnigrex-operations; printf journal-fixture > /native/omnigrex-operations/.cutover-marker; chmod 0600 /native/omnigrex-operations/.cutover-marker'
    # Never archive VM root disks, snapshots, or image caches as Session data.
    docker run --rm --network none --read-only --user 0:0 --entrypoint /bin/sh \
      --mount "type=volume,src=$namespace-msb,dst=/source,readonly" "$pi" \
      -ec 'cd /source; tar -czf - omnigrex-operations' > pi-backup/journal.tar.gz
    tar -tzf pi-backup/journal.tar.gz > /dev/null
  fi
  (cd pi-backup && shasum -a 256 *.dump *.tar.gz > SHA256SUMS && shasum -a 256 -c SHA256SUMS)
  # Recreate PostgreSQL containers and all writable data before restoring.
  compose down --timeout 60
  restore_volume "$namespace-runtime-state" < pi-backup/state.tar.gz
  if [ "$backend" = microsandbox ]; then
    # Preserve the image cache, replace only the protected journal.
    docker run --rm -i --network none --read-only --user 0:0 --entrypoint /bin/sh \
      --mount "type=volume,src=$namespace-msb,dst=/target" "$pi" \
      -ec 'rm -rf /target/omnigrex-operations; tar -xzf - -C /target' < pi-backup/journal.tar.gz
    docker run --rm --network none --read-only --user 10001:10001 --entrypoint /bin/sh \
      --mount "type=volume,src=$namespace-msb,dst=/native,readonly" "$pi" \
      -ec 'test "$(cat /native/omnigrex-operations/.cutover-marker)" = journal-fixture; test "$(stat -c %u:%g:%a /native/omnigrex-operations/.cutover-marker)" = 10001:10001:600'
  fi
  compose up -d --wait postgres
  compose exec -T postgres dropdb -U omnigrex --if-exists omnigrex
  compose exec -T postgres createdb -U omnigrex -O omnigrex omnigrex
  compose exec -T postgres pg_restore -U omnigrex -d omnigrex --exit-on-error --no-owner --no-acl < pi-backup/ledger.dump
  test "$(compose exec -T postgres psql -U omnigrex -d omnigrex -Atc 'SELECT value FROM cutover_pi_marker')" = fresh-pi-ledger
  docker run --rm --network none --read-only --user 10001:10001 --entrypoint /bin/sh \
    --mount "type=volume,src=$namespace-runtime-state,dst=/state,readonly" "$pi" -ec '
      test "$(cat /state/cutover-fixture/sessions/marker)" = session-fixture
      test "$(cat /state/workspace-captures/cutover-fixture/marker)" = capture-fixture
      test "$(stat -c %u:%g:%a /state/cutover-fixture/sessions/marker)" = 10001:10001:600'
  compose up --no-build --wait --wait-timeout 180
  compose exec -T orchestrator /usr/local/bin/omnigrex healthcheck
  printf 'PASS coordinated Pi ledger, state, capture and journal recovery (%s)\n' "$backend"
}
