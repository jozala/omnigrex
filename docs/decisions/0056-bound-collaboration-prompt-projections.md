---
status: accepted
date: 2026-11-01
decision-makers: [Omnigrex Developer]
---
# Bound collaboration prompt projections without truncating history

## Context

Durable clarification history grows independently of a single Agent Turn.
Embedding all accepted records in every prompt can exceed Pi's 1 MiB RPC-record ceiling before the agent can interpret input or compact its context.
Truncating accepted contents or delaying pending input would violate the Work Item collaboration contract.

## Decision

Keep immutable collaboration history durably in the collaboration aggregate, but cap its encoded prompt projection at 96 KiB before outer RPC escaping.
Do not duplicate the pending batch in historical input.
When the projection exceeds that cap, replace the embedded collections with frozen counts and explicit retrieval instructions.
Expose a scoped, read-only `get_collaboration_record` capability for both Roles, with ordered collection indices and lossless JSON-text chunks of at most 4096 Unicode characters.
All input accepted before preparation remains available to the same Turn; later input remains pending for another Turn.
The agent must retrieve every frozen pending-input record before interpretation or successful handoff and may retrieve older evidence as needed.

## Rationale

A small inline path preserves ordinary clarification ergonomics.
Bounded retrieval avoids transport failure regardless of accumulated discussion size, accounts for JSON escaping, and preserves authors, identities and exact content.
Silently dropping older records would remove evidence; scheduling a separate Turn per page would introduce an artificial batching delay and change Session scheduling.
Increasing Pi's RPC-record ceiling merely postpones the same failure and expands the runtime boundary unnecessarily.

## Consequences

Preparation and retrieval must agree on collection ordering and counts.
Large pending batches require additional read calls within the existing Turn and may need ordinary agent context management; no records are implicitly interpreted or resolved by retrieval.
The gateway still owns capability scope, and only the orchestrator schedules continuation.
The stored history remains unbounded by this projection policy; retention policy is a separate concern.

## Links

- [Work Item 163](https://github.com/jozala/omnigrex/issues/163)
- [Serialized clarification boundary](0055-serialize-work-item-clarification-at-turn-boundaries.md)
- [MCP mediation](../adr/0004-mediate-agent-capabilities-through-mcp.md)
