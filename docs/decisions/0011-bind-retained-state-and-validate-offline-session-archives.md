---
status: accepted
date: 2026-10-08
decision-makers: [Implementation agent]
---
# Bind retained state and validate offline Session archives

## Context

Retained-state attachment and collection coordination require logical ownership rather than physical storage paths.
Runtime storage is Participant-local and must remain bound to its execution and state contracts.

## Decision

Persist one installation Backend binding and immutable, versioned storage metadata for each Agent Participant.
A fresh installation claims its Backend family, contract version, and namespace on first composition.
Reject subsequent mismatches rather than reinterpreting state or falling back to another backend.

Use logical references binding the Participant resource, optional exact Agent Session, Backend contract, state format, Runtime Profile hash, and image.
Keep physical directory layouts behind storage adapters rather than exposing them to core coordination.
Production attachment uses the live Turn fence; collection resolves only irrevocably authorized Participant resources.
An initialized resource that disappears is unavailable protected state, not permission to create an empty replacement.

Export Session data as a version-1 tar containing regular files, directories, and a final manifest with ownership, contract identity, inventory, modes, sizes, and per-file SHA-256 checksums.
Bound content to 256 MiB, encoded archives to 384 MiB, inventory to 100,000 entries, and paths to 4,096 bytes.
Reject links, special files, known credential files, corrupt or incomplete inventory, and missing archive termination.
Restore into private staging, verify the complete archive, and publish atomically without overwriting different existing data.

## Rationale

Explicit binding prevents a configuration change from attaching another backend's state.
Preserving bound storage layouts maintains Session compatibility across disposable Execution Environments.
Backend-native environment snapshots would retain disposable execution and credential data and make restore vendor-specific.
Checksummed Session archives support compatible-host recovery without granting cross-backend migration authority.

## Consequences

Offline export and restore exclude Turn writers and collection through Store coordination and verify managed execution stop.
A complete recovery set still includes PostgreSQL and protected Workspace Captures; Session-only exports are supplemental.
The operator guide remains authoritative for coordinated backup and restore.

## Links

- [Execution Backend decision](0005-separate-execution-backends-from-coordination.md).
- [Retained-state contracts](../plans/2026-10-07-runtime-boundary-contracts.md#54-retained-state).
- [Step 06 implementation evidence](../plans/2026-10-08-retained-state-bindings.md).
