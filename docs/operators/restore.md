# Restore an installation

[Operator guide](README.md) · [Backup](backup.md)

## Before replacing data

Restore only a PostgreSQL dump, runtime-state archive, and applicable native journal from the same stopped-controller [backup window](backup.md).
The procedure below replaces current operational and Session state.
Use the original deployment `COMPOSE_FILE` with matching storage overrides and logical namespaces.

Restore matching `.env`, secrets, revision, compatibility artifacts, and every exact Runtime Profile image first.
The password secret must match the restored PostgreSQL role; `POSTGRES_PASSWORD_FILE` does not change an existing role password.
The replacement host must support the same guest platform, backend family/contract, namespace, and protected image/resource contracts.
For Microsandbox, provision all protected exact images into its native cache and restore the journal.
Keep the original host fenced off and both hosts closed to admission until replacement readiness and preflight pass.
Never run two writers against a recovery set.

## Restore

Select the backup directory below and export `BACKEND=docker` or `BACKEND=microsandbox` in the current shell.
Replace default volume names with the actual installation names in both backup and restore commands.
This fail-fast operation validates checksums and native absence before replacing protected state:

```sh
(
  set -eu
  BACKUP_DIR='/absolute/path/to/backups/20260907T120000Z'
  HELPER='alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b'
  case "${BACKEND:?set docker or microsandbox as in the backup shell}" in
    docker|microsandbox) ;;
    *) exit 1 ;;
  esac
  if [ "$BACKEND" = microsandbox ]; then
    test -f "$BACKUP_DIR/microsandbox-journal.tar.gz"
  fi

  (
    cd "$BACKUP_DIR"
    shasum -a 256 -c SHA256SUMS
  )
  docker compose stop -t 60 orchestrator

  RUNTIME_PROCESS_IDS="$(docker ps --quiet --filter label=io.omnigrex.runtime-process=true)"
  if [ -n "$RUNTIME_PROCESS_IDS" ]; then
    printf 'Managed Runtime Processes remain after shutdown:\n%s\n' "$RUNTIME_PROCESS_IDS" >&2
    exit 1
  fi

  if [ "$BACKEND" = microsandbox ]; then
    NATIVE_RESOURCES="$(docker compose run --rm --no-deps -T --entrypoint /usr/local/bin/msb orchestrator list --quiet)"
    if [ -n "$NATIVE_RESOURCES" ]; then
      printf 'Native resources remain after shutdown; resolve recovery before replacing state.\n' >&2
      exit 1
    fi
  fi
  docker compose down

  docker volume create omnigrex-runtime-state
  docker run --rm --network none --read-only \
    --mount type=volume,src=omnigrex-runtime-state,dst=/target \
    --mount "type=bind,src=$BACKUP_DIR,dst=/backup,readonly" \
    "$HELPER" sh -ec \
    'find /target -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +; tar -xzf /backup/runtime-state.tar.gz -C /target'

  if [ "$BACKEND" = microsandbox ]; then
    docker volume create omnigrex-microsandbox
    docker run --rm --network none --read-only \
      --mount type=volume,src=omnigrex-microsandbox,dst=/target \
      --mount "type=bind,src=$BACKUP_DIR,dst=/backup,readonly" \
      "$HELPER" sh -ec 'rm -rf /target/omnigrex-operations; tar -xzf /backup/microsandbox-journal.tar.gz -C /target'
  fi

  docker compose up -d --wait postgres
  docker compose exec -T postgres dropdb --username=omnigrex --if-exists omnigrex
  docker compose exec -T postgres createdb --username=omnigrex --owner=omnigrex omnigrex
  docker compose exec -T postgres \
    pg_restore --username=omnigrex --dbname=omnigrex \
    --exit-on-error --no-owner --no-acl \
    < "$BACKUP_DIR/omnigrex.dump"
)
```

An unavailable native inventory aborts just like a nonempty one.
It is not evidence of absence.
Preserve the current journal and resolve remaining native resources through unchanged-configuration recovery before retrying.
Never use `docker compose down --volumes` for ordinary deployment, backup, or restore; it permanently removes declared named volumes.

## Verify the replacement

After matching configuration and protected images are in place, start the stack and run preflight for every repository:

```sh
docker compose up --wait
docker compose exec orchestrator \
  /usr/local/bin/omnigrex doctor --repository OWNER/REPOSITORY
```

Require readiness, all-pass repository diagnostics, and real webhook delivery verification before reopening admission.
Retained Sessions cannot continue when their exact digests are missing.
Preserve the original logical namespace instead of remapping state to new names.

## Restore a supplemental Session export

After restoring the matching PostgreSQL ledger and backend namespace, restore a supplemental archive only into an absent Participant state resource.
Keep the controller and all writers stopped under the same offline barrier as [export](backup.md#supplemental-session-exports).

```sh
PARTICIPANT='<canonical-Agent-Participant-UUID>'
docker compose run --rm --no-deps -T --entrypoint /usr/local/bin/omnigrex \
  orchestrator state restore --participant "$PARTICIPANT" < "session-${PARTICIPANT}.tar"
```

The complete archive is validated in private staging before atomic publication.
A different Participant, backend, namespace, state format, Runtime Profile, or image binding is rejected.
Retrying the same completed restore succeeds; other existing state is not overwritten or merged.
This does not convert conversations, remap namespaces, or enable cross-backend migration.
Restart the unchanged deployment and run preflight as above.

## Rehearse recovery

Perform drills in a non-production environment.
A complete drill reactivates a retained test Work Item and observes the same Participant, Session/runtime identity, and image binding in a fresh Execution Environment.
Confirm History Replay without a new model prompt using the [state recovery and upgrade gates](../contributors/runtime-qualification.md#pi-upgrade-and-state-recovery-qualification).
There is currently no operator-facing History Replay UI.
Fixture archive round-trips alone do not establish production Session Continuation.
