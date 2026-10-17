# Sourced by the isolated Pi deployment smoke with its exact fixture resources.
archive_volume() {
  docker run --rm --network none --read-only --user 0:0 --entrypoint /bin/sh \
    --mount "type=volume,src=$1,dst=/source,readonly" "$pi" -ec 'cd /source; tar -czf - .'
}

restore_volume() {
  docker run --rm -i --network none --read-only --user 0:0 --entrypoint /bin/sh \
    --mount "type=volume,src=$1,dst=/target" "$pi" \
    -ec 'find /target -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +; tar -xzf - -C /target'
}

verify_pi_recovery() {
  compose stop orchestrator
  remaining="$(docker ps --quiet --filter label=io.omnigrex.runtime-process=true --filter "volume=$namespace-runtime-state")"
  test -z "$remaining"
  if [ "$backend" = microsandbox ]; then
    remaining="$(compose run --rm --no-deps -T --entrypoint /usr/local/bin/msb orchestrator list --quiet)"
    test -z "$remaining"
  fi
  # Synthetic markers qualify coordinated dump/archive mechanics and ownership.
  # Actual Session continuation/replay is qualified by the Workflow and upgrade gates.
  compose exec -T postgres psql -U omnigrex -d omnigrex -v ON_ERROR_STOP=1 \
    -c 'CREATE TABLE pi_recovery_marker (value text); INSERT INTO pi_recovery_marker VALUES ($$pi-ledger$$)'
  docker run --rm --network none --read-only --user 10001:10001 --entrypoint /bin/sh \
    --mount "type=volume,src=$namespace-runtime-state,dst=/state" "$pi" -ec '
      mkdir -p /state/recovery-fixture/sessions /state/workspace-captures/recovery-fixture
      printf session-fixture > /state/recovery-fixture/sessions/marker
      printf capture-fixture > /state/workspace-captures/recovery-fixture/marker
      chmod 0600 /state/recovery-fixture/sessions/marker /state/workspace-captures/recovery-fixture/marker'
  mkdir -m 0700 pi-backup
  compose exec -T postgres pg_dump -U omnigrex -d omnigrex -Fc --no-owner --no-acl > pi-backup/ledger.dump
  compose exec -T postgres pg_restore --list < pi-backup/ledger.dump > /dev/null
  archive_volume "$namespace-runtime-state" > pi-backup/state.tar.gz
  tar -tzf pi-backup/state.tar.gz > /dev/null
  if [ "$backend" = microsandbox ]; then
    # Diagnostic-only fixtures have no production dispatch records yet.
    docker run --rm --network none --read-only --user 10001:10001 --entrypoint /bin/sh \
      --mount "type=volume,src=$namespace-msb,dst=/native" "$pi" \
      -ec 'mkdir -p /native/omnigrex-operations; printf journal-fixture > /native/omnigrex-operations/.recovery-marker; chmod 0600 /native/omnigrex-operations/.recovery-marker'
    docker run --rm --network none --read-only --user 0:0 --entrypoint /bin/sh \
      --mount "type=volume,src=$namespace-msb,dst=/source,readonly" "$pi" \
      -ec 'cd /source; tar -czf - omnigrex-operations' > pi-backup/journal.tar.gz
    tar -tzf pi-backup/journal.tar.gz > /dev/null
  fi
  (cd pi-backup && shasum -a 256 *.dump *.tar.gz > SHA256SUMS && shasum -a 256 -c SHA256SUMS)
  compose down --timeout 60
  restore_volume "$namespace-runtime-state" < pi-backup/state.tar.gz
  if [ "$backend" = microsandbox ]; then
    docker run --rm -i --network none --read-only --user 0:0 --entrypoint /bin/sh \
      --mount "type=volume,src=$namespace-msb,dst=/target" "$pi" \
      -ec 'rm -rf /target/omnigrex-operations; tar -xzf - -C /target' < pi-backup/journal.tar.gz
    docker run --rm --network none --read-only --user 10001:10001 --entrypoint /bin/sh \
      --mount "type=volume,src=$namespace-msb,dst=/native,readonly" "$pi" \
      -ec 'test "$(cat /native/omnigrex-operations/.recovery-marker)" = journal-fixture; test "$(stat -c %u:%g:%a /native/omnigrex-operations/.recovery-marker)" = 10001:10001:600'
  fi
  compose up -d --wait postgres
  compose exec -T postgres dropdb -U omnigrex --if-exists omnigrex
  compose exec -T postgres createdb -U omnigrex -O omnigrex omnigrex
  compose exec -T postgres pg_restore -U omnigrex -d omnigrex --exit-on-error --no-owner --no-acl < pi-backup/ledger.dump
  test "$(compose exec -T postgres psql -U omnigrex -d omnigrex -Atc 'SELECT value FROM pi_recovery_marker')" = pi-ledger
  docker run --rm --network none --read-only --user 10001:10001 --entrypoint /bin/sh \
    --mount "type=volume,src=$namespace-runtime-state,dst=/state,readonly" "$pi" -ec '
      test "$(cat /state/recovery-fixture/sessions/marker)" = session-fixture
      test "$(cat /state/workspace-captures/recovery-fixture/marker)" = capture-fixture
      test "$(stat -c %u:%g:%a /state/recovery-fixture/sessions/marker)" = 10001:10001:600'
  compose up --no-build --wait --wait-timeout 180
  compose exec -T orchestrator /usr/local/bin/omnigrex healthcheck
  printf 'PASS coordinated Pi ledger, state, capture and journal recovery (%s)\n' "$backend"
}
