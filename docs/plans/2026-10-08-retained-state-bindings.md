# Retained Runtime State bindings

## Scope and prerequisite

This records Step 06 of the [combined runtime plan](2026-10-05-pi-and-pluggable-execution-backends.md), following [location-independent workspace capture](2026-10-08-location-independent-workspaces.md).
OpenCode remains the production agent runtime and Docker remains the production Execution Backend.
[Decision 0011](../decisions/0011-bind-retained-state-and-validate-offline-session-archives.md) records the storage transition and initial Session archive format.

## Implemented boundary

- `execution.StateRef` binds a logical resource to its Agent Participant, optional exact Agent Session, Backend namespace and contract version, state format, Runtime Profile hash, and image.
- Migration 000032 preserves existing Session identities and storage locations while adding an immutable installation Backend binding and per-Participant versioned adapter metadata.
- Existing data is Docker-owned; composition claims the configured namespace once.
- New installations claim their Backend family, version, and namespace on first composition.
- Production launch resolves the Session reference under the exact live Turn fence, ensures its availability outside the database transaction, and records initialization under a fresh fence before dispatch.
- Docker validates ownership, Backend namespace, format, and metadata before physical access and checks protected availability again before creation.
- Initialized data that disappears fails explicitly instead of silently creating a new conversation store.
- Production Session preparation receives a logical reference rather than a physical state path.
- Retention workers collect Participant identities; the state adapter resolves their immutable resources under Store collection authorization.
- Readiness checks protected state availability as well as the existing exact-image checks.
- Resources already irrevocably authorized for collection are excluded from protected-state readiness while physical deletion awaits its durable acknowledgement.

The existing admission, stop, recovery, and settlement barriers remain authoritative for exclusive epoch-bound execution.
A stale Turn cannot acquire or acknowledge state initialization.
Revoking an MCP token or closing a transport does not grant a successor writer.
The bounded Docker collection helper remains private to the storage adapter, and collection is idempotent for its exact immutable resource.
Participant tool caches and protected captures still follow their existing cleanup policy after state collection succeeds.

## Archive and restore contract

`omnigrex state export` and `omnigrex state restore` are deliberate offline application entry points for the neutral Session recovery operations.
They resolve the exact Participant reference from PostgreSQL and exclude active Turns, human prompt leases, irreversible collection, and unresolved dispatched execution creation.
Their Store recovery boundary prevents new writer admission and collection while the operation runs.
The callback consumes an already-read metadata snapshot, avoiding another pool connection while holding that boundary.
Docker separately rejects managed environments that are running, created, or have unavailable stop evidence.
Offline commands have a ten-minute deadline and interrupt blocked archive pipes on cancellation.

The version-1 tar contains only regular Session files and directories, followed by a manifest recording exact logical ownership, contract identity, inventory, permissions, byte counts, and per-file SHA-256.
Limits are 256 MiB of file content, 384 MiB of encoded archive, 100,000 entries, 4,096-byte paths, and a 16 MiB manifest.
The adapter rejects traversal, duplicate inventory, links, devices, known credential filenames, inconsistent checksums, unsupported versions, partial transfers, missing terminators, and hidden trailing data.
Restore validates into a private sibling directory before atomic publication.
Restoration applies exact file permissions independently of the caller's umask and synchronizes files and directory entries before publishing the staged resource.
Repeating the same successful restore is idempotent; different existing data is not overwritten or merged.
Unpublished staging is removed on every return path, repairing restrictive permissions only within the disposable tree and reporting cleanup failures.
Authorized Participant collection also removes abandoned `.restore-<32 lowercase hexadecimal digits>` siblings from interrupted restores while preserving unrelated entries and other Participants' resources.

Provider credentials retain the existing ephemeral OpenCode environment supply.
State exports exclude Turn scratch, tool caches, workspaces, captures, and executable environment snapshots.
The full coordinated PostgreSQL/state-volume backup remains the authoritative recovery set and includes protected captures.
See the [backup](../operators/backup.md) and [restore](../operators/restore.md) guides for coordinated recovery and supplemental Session procedures.

## Verification

Focused coverage includes:

- `TestRetainedStateMigrationPreservesLegacySessionBinding`: forward-only upgrade from schema 31 preserves ACP identity and storage location and does not guess a deployment namespace.
- `TestRetainedStateBindingsPersistAndFenceWriters`: durable bindings across Store instances, family/version/namespace mismatch, Session ownership, unavailable initialized data, protected collection, active recovery rejection, and expired Turn fences.
- `TestRetainedStateOfflineRecoveryExcludesCollectionAndConcurrentWriters`: recovery exclusion and immutable Backend/storage metadata.
- `TestRetainedStateCollectionKeepsNeighborProtectedAndReadinessAvailable`: real Store collection authorization, idempotent physical deletion, neighboring protection, and readiness between deletion and acknowledgement.
- Existing single-connection Launcher tests now exercise the production logical state preparation route.
- `TestRetainedStateRoundTripIsolationAndCollection`: compatible-host restore, inventory and modes, idempotent restore/collection, and neighboring Participant preservation.
- `TestRetainedStateRestorePreservesPermissionsUnderPrivateUmask`: private-backup defaults do not alter restored file modes or break idempotent retry.
- `TestRetainedStateRestoreRetryRemovesRestrictiveStaging`: identical and conflicting restores leave no Session-data copies even when archived directories have mode `0500`.
- `TestRetainedStateRestoreReportsStagingCleanupFailure`: an actual filesystem deletion failure is reported without changing retained Session data.
- `TestRetainedStateCollectionRemovesInterruptedRestoreCopies`: authorized, idempotent collection removes abandoned copies with mode `0000` directories, unlinks staging symlinks without following them, and preserves unrelated and neighboring resources.
- Archive rejection tests prove that corrupt, partial, unsafe, or credential-bearing input never publishes partial state.
- `TestRetainedStateDockerConformance`: the real named-volume adapter exports OpenCode Session state, removes the disposable environment and original state directory, restores the archive, and continues the exact Session in another environment.
- The Docker conformance test also rejects backup with a live writer, detects provider-credential persistence, denies another Participant access to the Session, protects retained state, and proves idempotent collection without removing neighboring state.
- The non-root Docker conformance fixture repeats restrictive-directory restores and exercises the production collection helper against retained data and interrupted restore staging.

The Docker fixture mounts the actual named volume into an isolated trusted control-plane helper so the same test works on Docker Desktop and Linux.
Only that trusted helper receives Docker management access; the agent environments receive their isolated state subpaths and disposable mounts.

The following verification passed on the development Docker Engine:

```sh
mise install
go test -race ./internal/runtime/docker ./cmd/omnigrex
go test -race ./internal/runtime/docker -run '^TestRetainedState' -count=1
go test -race -tags=integration ./internal/store -run '^TestRetainedState' -count=1 -timeout 3m
go test -race -tags=integration ./internal/runtime/docker -run '^TestRetainedStateDockerConformance$' -count=1 -timeout 5m
mise run check
mise run test-integration
```

The final local gate passed formatting, static analysis, production reachability, race-detected unit tests, dependency checks, and both Compose configuration validations.
The final Docker-backed gate passed doctor, OpenCode ACP, Docker, Store, and deployment suites, including the fresh-environment Session restore proof.
Independent Standards and Spec reviews found no actionable findings after the restrictive-permission staging cleanup correction.
The destructive Compose resource-recreation gate was not run.

## Transitional ownership and next step

Legacy `runtime_state_path` database fields remain for existing OpenCode identity and retention compatibility; production physical access resolves the versioned adapter metadata.
Direct OpenCode qualification fixtures retain their old path-based preparation route when they deliberately omit the state service.
Remove those compatibility fields and paths with OpenCode in Step 17, rather than migrating conversations during this storage change.
The installation Backend binding requires explicit state disposal or a separately implemented migration before changing family or namespace.
The Microsandbox implementation and its shared state conformance are delivered in Step 08.
Step 07 establishes repository image and in-environment tool provisioning contracts while preserving this state separation.
