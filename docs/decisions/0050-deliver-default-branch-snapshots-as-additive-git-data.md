---
status: accepted
date: 2026-10-27
decision-makers: [Omnigrex Developer for Work Item 147]
---
# Deliver default-branch snapshots as additive Git data

## Context

[Work Item 147](https://github.com/jozala/omnigrex/issues/147) requires freshly observed default-branch objects during an Agent Turn, without credentials in the Runtime Process or changes to the Change Proposal.
The Issue discussion selects active-checkout delivery and instruction-level final freshness rather than atomic merge admission.

## Decision

Expose `refresh_default_branch` as a scoped, ledgered local mutation for both Roles, using Orchestrator credential authority.
Acquire the exact observed commit and complete ancestry in a fresh controller-owned bare repository.
Deliver only a verified pack, index, and operation-specific `refs/omnigrex/default/<digest>` reference.
The digest binds the durable mutation UUID to the repository and exact Participant, Session, Turn, and epoch.
Never reuse preparation or run authenticated Git against agent-controlled metadata.

The backend-neutral installer accepts credential-free snapshot bytes, not a host path.
Docker pauses the exact managed container and validates its workspace archive before copying additive files through the daemon.
Microsandbox uses its exact-execution lock, pause-with-flush, verified backing identity, and rooted additive file installation.
The gateway holds durable Turn authority through the bounded installation and resumption; existing mutation serialization coordinates publication, and backend serialization coordinates capture.
Linked Git metadata, alternate object stores, grafts, replacement refs, partial repositories, active lock files, and conflicting destinations fail closed.
Existing files and directories are never overwritten.

Observe the default branch again after delivery without chasing it inside the call.
Return explicit `current`, `stale`, `default_branch_changed`, or `unavailable` status and observation times.
A replay returns the recorded result, not fresh evidence; a new observation requires a new operation identity.
An interrupted or ambiguous installation remains unresolved under existing mutation recovery rather than fetching a different snapshot or claiming success from partially applied files.

## Rationale

Credentialed fetch in the active checkout would execute untrusted configuration and expose network authority.
Replacing the checkout would discard work and Session continuity.
A separate verification checkout would not support ordinary Developer conflict resolution in the active workspace.
An immutable operation reference avoids changing cached remote refs or rebinding publication and Reviewer authority.

## Consequences

The agent still performs mergeability checks and resolves conflicts under its existing Role permissions.
Snapshots are observations, not merge admission or Stage completion.
Acquisition and transfer limits can produce unavailable verification; unsupported metadata or interrupted delivery requires existing blocker/recovery handling rather than destructive repair.
Native backend and Pi-discovery qualification remains required before delivery is considered complete.

## Links

- [MCP authority](../adr/0004-mediate-agent-capabilities-through-mcp.md)
- [Operation-selected credentials](../adr/0009-select-github-app-credentials-by-operation.md)
- [Location-independent workspace evidence](0008-keep-workspace-capture-separate-from-publication-policy.md)

## Installation-fence refinement

[Decision 0051](0051-exclude-execution-cleanup-without-locking-turn-leases.md) replaces the original long-held Turn row-lock fence with an execution-reservation guard so heartbeats can continue during installation.
