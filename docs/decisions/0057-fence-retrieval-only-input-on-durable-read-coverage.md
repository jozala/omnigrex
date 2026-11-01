---
status: accepted
date: 2026-11-01
decision-makers: [Omnigrex Developer]
---
# Fence retrieval-only input on durable read coverage

## Context

Bounded collaboration projections omit pending contents when the inline budget is exceeded.
Assigning those records to a frozen Turn does not establish delivery, and instructions alone cannot enforce the pending-input handoff fence.

## Decision

Distinguish frozen Turn assignment from delivery with a durable pending-delivery marker on omitted input.
Keep that input pending for mutation admission, settlement, and recovery until every canonical JSON-text chunk has a successful scoped read recorded under the live Turn/Workflow fence.
Use the existing immutable read ledger as progress evidence, bound to the Turn, collection, record index, and full-record SHA-256 fingerprint.
Count distinct canonical offsets, not call count or receipt of the final chunk.
Validate each successful chunk against the controller's scoped record before admitting coverage, and commit complete delivery atomically with the read record.
Keep unread records eligible for the retained Session's next preparation if the Turn ends early; preserve normal inline delivery and existing budget semantics.

## Rationale

A separate agent acknowledgment would prove only an assertion and expand the mutation surface without proving access to the contents.
Keeping independent delivery checkpoints per chunk duplicates the already durable read ledger.
Rewriting the entire collaboration aggregate for every chunk makes large discussions unnecessarily expensive; only completed record delivery changes its marker.
Duplicate, out-of-order, failed, unrecorded, or partial retrieval cannot silently release the fence.
Neither successful retrieval nor its fingerprint constitutes interpretation or implementation authorization.

## Consequences

Agents follow canonical offsets from zero and concatenate chunks with one fingerprint per record.
Controller restart preserves recorded progress; a new Turn conservatively re-delivers unread records, while already delivered records remain history.
The orchestrator remains the sole continuation owner, and human response bodies and provenance remain immutable.

## Links

- [Work Item 163](https://github.com/jozala/omnigrex/issues/163)
- [Bound collaboration prompt projections](0056-bound-collaboration-prompt-projections.md)
- [Serialized clarification boundary](0055-serialize-work-item-clarification-at-turn-boundaries.md)
- [MCP mediation](../adr/0004-mediate-agent-capabilities-through-mcp.md)
