# Back up an installation

[Operator guide](README.md) · [Restore](restore.md)

## Recovery-set requirements

PostgreSQL and the runtime-state volume form one coordinated recovery set, plus the protected native journal for Microsandbox.
The ledger contains canonical Runtime Profile contracts and immutable backend/image ownership.
Runtime state contains Pi Session data and sealed Workspace Captures under `workspace-captures`, mounted only into the orchestrator at `/var/lib/omnigrex/runtime-state/workspace-captures`.
Captures preserve publication and terminal evidence after the original execution and live workspace disappear.
They are protected Participant data, not disposable workspace cache.
See the [workspace evidence contract](../plans/2026-10-08-location-independent-workspaces.md).

Use the exact deployment `COMPOSE_FILE` and matching storage names throughout this procedure.
Do not copy state while an Agent Turn is active.
Supplemental Session exports alone are not a complete recovery set.

## Stop writers

Stop accepting work at ingress and stop the controller gracefully with a grace period covering configured cleanup budgets.
Verify no managed Runtime Process remains:

```sh
docker compose stop -t 60 orchestrator
docker ps --filter label=io.omnigrex.runtime-process=true --format '{{.ID}} {{.Names}}'
```

The second command must produce no output.
If a process remains, investigate and allow unchanged-configuration startup recovery to resolve exact executions, then stop again.
Do not archive unresolved state.

For Microsandbox, inspect native inventory with the controller stopped:

```sh
docker compose run --rm --no-deps -T --entrypoint /usr/local/bin/msb orchestrator list --quiet
```

Require no native resources before continuing.
A stopped control-plane container does not prove guest execution or an ambiguous dispatch is resolved.
Do not discard started environments or journals to bypass missing stop/writeback evidence.
During compatible-host restore, keep the original host fenced from ingress and human-prompt access.

## Create the coordinated recovery set

Set `BACKEND=docker` or `BACKEND=microsandbox` in the current shell and export it.
Replace default volume names below with the installation's actual names.
This example creates a private unique directory and publishes each archive only after validation:

```sh
(
  set -eu
  umask 077
  BACKUP_ROOT="$PWD/backups"
  BACKUP_DIR="$BACKUP_ROOT/$(date -u +%Y%m%dT%H%M%SZ)"
  install -m 0700 -d "$BACKUP_ROOT"
  mkdir -m 0700 "$BACKUP_DIR"
  cleanup_failed_backup() { rm -rf -- "$BACKUP_DIR"; }
  trap cleanup_failed_backup EXIT
  trap 'exit 1' HUP INT TERM

  docker compose exec -T postgres \
    pg_dump --username=omnigrex --dbname=omnigrex \
    --format=custom --no-owner --no-acl \
    > "$BACKUP_DIR/omnigrex.dump.tmp"
  docker compose exec -T postgres \
    pg_restore --list < "$BACKUP_DIR/omnigrex.dump.tmp" > /dev/null
  mv "$BACKUP_DIR/omnigrex.dump.tmp" "$BACKUP_DIR/omnigrex.dump"

  : "${BACKUP_DIR:?BACKUP_DIR must be set}"
  test -d "$BACKUP_DIR"
  HELPER='alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b'
  STATE_VOLUME=omnigrex-runtime-state # replace with this installation's exact volume name
  docker run --rm --network none --read-only \
    --mount "type=volume,src=$STATE_VOLUME,dst=/source,readonly" \
    "$HELPER" sh -ec 'cd /source; tar -czf - .' \
    > "$BACKUP_DIR/runtime-state.tar.gz.tmp"
  tar -tzf "$BACKUP_DIR/runtime-state.tar.gz.tmp" > /dev/null
  mv "$BACKUP_DIR/runtime-state.tar.gz.tmp" "$BACKUP_DIR/runtime-state.tar.gz"

  # Preserve the private native journal, not disposable VM images.
  : "${BACKEND:?set the selected deployment backend}"
  if [ "$BACKEND" = microsandbox ]; then
    NATIVE_VOLUME=omnigrex-microsandbox # replace with this installation's exact native volume
    TURN_COUNT="$(docker compose exec -T postgres psql --username=omnigrex --dbname=omnigrex -Atc 'SELECT count(*) FROM agent_turns')"
    docker run --rm --network none --read-only \
      --mount "type=volume,src=$NATIVE_VOLUME,dst=/source,readonly" \
      --env "TURN_COUNT=$TURN_COUNT" \
      "$HELPER" sh -ec 'cd /source; if test -d omnigrex-operations; then tar -czf - omnigrex-operations; else test "$TURN_COUNT" = 0; dd if=/dev/zero bs=1024 count=1 2>/dev/null | gzip; fi' \
      > "$BACKUP_DIR/microsandbox-journal.tar.gz.tmp"
    tar -tzf "$BACKUP_DIR/microsandbox-journal.tar.gz.tmp" > /dev/null
    mv "$BACKUP_DIR/microsandbox-journal.tar.gz.tmp" "$BACKUP_DIR/microsandbox-journal.tar.gz"
  elif [ "$BACKEND" != docker ]; then
    exit 1
  fi

  (
    cd "$BACKUP_DIR"
    shasum -a 256 omnigrex.dump *.tar.gz > SHA256SUMS.tmp
    mv SHA256SUMS.tmp SHA256SUMS
  )

  trap - EXIT HUP INT TERM
  printf 'Recovery set: %s\n' "$BACKUP_DIR"
)
```

The archive preserves numeric ownership, including UID/GID 10001.
Workspace and mise volumes are reproducible caches and are unnecessary for disaster recovery when no Turn is active.
Native image cache is re-provisioned from exact registry artifacts; disposable root disks, snapshots, tmpfs credentials, and scratch are excluded.
The immutable registry-document cache under `omnigrex-registry-documents` is likewise reproducible on demand and excluded from the recovery set.
A diagnostic-only installation may have no native journal only when the stopped ledger has no Agent Turns.
An existing Turn with an absent journal makes backup fail rather than silently omit required metadata.
Archive disposable resources separately only if local policy requires a complete host checkpoint.

Back up `.env`, secrets, compatibility artifacts, deployed commit, and every referenced image digest separately in encrypted storage.
Include the controller-only subscription credential directory using [its stopped-writer backup procedure](provider-authentication.md#renewal-and-storage).
Do not put them in the same unencrypted directory as operational exports.

Restart the unchanged deployment, run repository preflight, then reopen ingress and human-trigger admission:

```sh
docker compose up -d --wait orchestrator
```

## Supplemental Session exports

Use `omnigrex state export --participant UUID` for one Participant's checksummed, ownership-bound Pi Session archive.
Use the exact Participant UUID from the ledger, not a Role, Profile name, or runtime Session ID.
It excludes PostgreSQL and protected Workspace Captures, so it does not replace the coordinated recovery set.

Stop the controller and prove managed executions stopped as above.
Export also rejects active Turns, human-prompt leases, irreversible collection, unresolved creation, and environments without confirmed stop evidence.
Each command has a ten-minute deadline, including blocked pipes and database operations.

Run with the same Compose selection and mounted state, publishing only successful output:

```sh
(
  set -eu
  umask 077
  PARTICIPANT='<canonical-Agent-Participant-UUID>'
  ARCHIVE="session-${PARTICIPANT}.tar"
  trap 'rm -f -- "$ARCHIVE.tmp"' EXIT HUP INT TERM
  docker compose run --rm --no-deps -T --entrypoint /usr/local/bin/omnigrex \
    orchestrator state export --participant "$PARTICIPANT" > "$ARCHIVE.tmp"
  mv "$ARCHIVE.tmp" "$ARCHIVE"
  trap - EXIT HUP INT TERM
)
```

The manifest binds exact Backend and Runtime Profile identity, inventory, permissions, and per-file SHA-256 checksums.
Links, special files, known credential files, partial data, and unsupported formats are rejected.
Limits are 256 MiB file content, 384 MiB encoded archive, and 100,000 entries.
Keep exports private.
Restore them only after restoring the matching ledger and namespace using [supplemental restore](restore.md#restore-a-supplemental-session-export).

Next: [Restore and drill the recovery set](restore.md).
