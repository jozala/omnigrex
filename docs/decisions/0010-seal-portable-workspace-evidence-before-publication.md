---
status: accepted
date: 2026-10-08
decision-makers: [Implementation agent]
---
# Seal portable workspace evidence before publication

## Context

Decision 0008 requires consistent publication and terminal Workspace Captures, but leaves their encoding and backend-private capture mechanism to Step 05.
The mid-prompt experiments show that Docker suspension and Microsandbox flushed pause preserve an outstanding authenticated MCP interaction.
They also show that Microsandbox host files do not necessarily have the guest-visible modes or symlink types.

## Decision

Use version 1 manifests binding a capture to its full execution identity, purpose, operation, and SHA-256 of a normalized tar archive.
The trusted backend source must preserve guest-visible filesystem semantics and keep every workspace writer quiescent throughout transfer.
The initial Docker implementation uses its archive API while the exact container is paused, or after confirmed stop for terminal evidence.
Capture includes the complete Git directory and working-tree inventory, including ignored files, rather than selecting files through agent-controlled Git configuration.
Limit each source and normalized archive to 1 GiB, 100,000 entries, and 4,096-byte paths or link targets.
Reject unsupported entries and exceeded limits explicitly.

Store archives outside agent-mounted subpaths in the existing retained-state volume, and retain them through the Participant's protected retention boundary.
The original Turn identity deterministically locates terminal evidence after restart without inspecting a replacement workspace.
A sealed operation with missing or corrupt evidence must fail; it cannot capture newer content under the same operation identity.
Persist its identity and digest independently in PostgreSQL and verify the artifact against that record on access.
Corroborate against a fresh trusted checkout of the terminal intent's exact published SHA, rather than the current contents of a disposable publication cache.

## Rationale

A directory copy without writer suspension can combine incompatible observations.
An agent-authored cleanliness report cannot establish publication preconditions.
Selecting only Git-tracked files risks concealing staged or untracked changes and trusting repository-controlled exclusions.
A complete bounded archive keeps the first format simple and independently inspectable while failing safely for unusually large workspaces.
Backend-native environment snapshots would include unrelated execution or credential state and would couple trusted publication to a vendor format.

## Consequences

Publication inspects a private, verified materialization with executable configuration and history indirection neutralized before trusted Git runs.
Finalization and recovery preserve the stopped environment when a required capture cannot be sealed.
Backups must include protected captures with retained Agent Sessions; the operator guide remains authoritative.
Microsandbox's strict guest-stat virtualization requires translation of its versioned metadata before supplying the common archive.
[Decision 0013](0013-use-the-microsandbox-go-sdk-and-posix-bindings.md) refines this mechanism by qualifying literal POSIX bindings with guest-stat virtualization disabled.
More selective inventory encoding or different limits require a versioned refinement rather than silent omission.

## Links

- [Decision 0008](0008-keep-workspace-capture-separate-from-publication-policy.md).
- [Step 05 implementation and capture proof](../plans/2026-10-08-location-independent-workspaces.md).
- [Workspace contracts](../plans/2026-10-07-runtime-boundary-contracts.md#5-workspace-and-retained-state-contracts).
