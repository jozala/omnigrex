---
status: accepted
date: 2026-10-07
decision-makers: [Mariusz]
---
# Keep workspace capture separate from publication policy

## Context

Publication, outcome reconciliation, and delayed corroboration currently inspect local workspace paths.
An interface around the GitHub push alone would not make a remote execution backend substitutable.
The agent may also change its workspace after publishing, so a publication-time observation cannot prove its final state.

## Decision

Make workspace preparation, consistent immutable capture, access, and discard location-independent operations.
Keep Git history validation, branch preconditions, GitHub credentials, and publication decisions in the trusted orchestrator-side service.
Both local and future remote implementations supply evidence through the same capture contract.

Distinguish publication captures from fresh terminal captures.
Retain the latter through settlement or deferred corroboration when they are needed as evidence.
The trusted capture mechanism must establish consistency and preserve the inputs needed for existing cleanliness/history checks; an agent's claim that the workspace is clean is insufficient.

## Rationale

Local adapters can continue using local storage internally without exposing live host directories to core workers.
A future remote adapter can transfer the same logical artifact while publication remains local.
Moving publication into the sandbox would distribute GitHub authority and duplicate policy.
Reusing an earlier snapshot would conceal later edits.

## Consequences

Cleanup must distinguish stopping execution from deleting data required for terminal observation.
Mid-prompt capture must not deadlock on the MCP call it is serving.
Consistent capture is a mandatory conformance gate, not behavior already proved by the initial microVM and Pi experiments.
The exact capture/transfer mechanism remains backend-private; the common artifact validation and publication policy do not.

## Links

- [Workspace contracts and sequencing](../plans/2026-10-07-runtime-boundary-contracts.md#5-workspace-and-retained-state-contracts)
- [Combined implementation plan](../plans/2026-10-05-pi-and-pluggable-execution-backends.md)
- [Existing verified publication design](../plans/2026-09-28-verified-multi-commit-publication-design.md)
