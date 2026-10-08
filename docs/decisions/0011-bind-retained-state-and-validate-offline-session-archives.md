---
status: accepted
date: 2026-10-08
decision-makers: [OpenCode implementation agent]
---
# Bind retained state and validate offline Session archives

## Context

Step 06 must preserve the existing Docker/OpenCode deployment while removing physical state paths from production attachment and collection coordination.
Existing OpenCode storage is Participant-local and can contain multiple Agent Sessions.
SQL cannot infer the configured Docker storage namespace during migration.

## Decision

Persist one installation Backend binding and immutable, versioned storage metadata for each Agent Participant.
An existing installation is explicitly Docker-owned; composition claims its previously unspecified namespace once.
A fresh installation claims its Backend family, contract version, and namespace on first composition.
Reject subsequent mismatches rather than reinterpreting state or falling back to another backend.

Use logical references binding the Participant resource, optional exact Agent Session, Backend contract, state format, Runtime Profile hash, and image.
Keep the existing OpenCode directory layout behind the Docker adapter rather than copying or migrating conversations.
Production attachment uses the live Turn fence; collection resolves only irrevocably authorized Participant resources.
An initialized resource that disappears is unavailable protected state, not permission to create an empty replacement.

Export Session data as a version-1 tar containing regular files, directories, and a final manifest with ownership, contract identity, inventory, modes, sizes, and per-file SHA-256 checksums.
Bound content to 256 MiB, encoded archives to 384 MiB, inventory to 100,000 entries, and paths to 4,096 bytes.
Reject links, special files, known credential files, corrupt or incomplete inventory, and missing archive termination.
Restore into private staging, verify the complete archive, and publish atomically without overwriting different existing data.

## Rationale

Explicit binding prevents a configuration change from attaching another backend's state.
Keeping the current layout preserves operational continuity without conflating storage refactoring with the later Pi cutover.
Backend-native environment snapshots would retain disposable execution and credential data and make restore vendor-specific.
Checksummed Session archives support compatible-host recovery without granting cross-backend migration authority.

## Consequences

Offline export and restore exclude Turn writers and collection through Store coordination and verify managed Docker execution stop.
A complete recovery set still includes PostgreSQL and protected Workspace Captures; Session-only exports are supplemental.
The operator guide remains authoritative for coordinated backup and restore.
Legacy path fields and direct OpenCode qualification fixtures remain transitional until the explicit OpenCode removal step.

## Links

- [Execution Backend decision](0005-separate-execution-backends-from-coordination.md).
- [Retained-state contracts](../plans/2026-10-07-runtime-boundary-contracts.md#54-retained-state).
- [Step 06 implementation evidence](../plans/2026-10-08-retained-state-bindings.md).
