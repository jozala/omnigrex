---
status: accepted
date: 2026-10-24
decision-makers: [Omnigrex implementation agent]
---
# Derive publication read targets without rebinding Turn authority

## Context

A Developer can publish multiple commits and open a Change Proposal within one Agent Turn, but the starting authority can contain neither that head nor that Pull Request.
Frozen reads prevent inspection of the Developer's own published work, while following the branch's current tip would authorize unrelated external pushes.
The publication planning maps already track progress but cannot independently establish durable read access after backend reconstruction or mutation replay.

## Decision

Keep token authority, mutation scopes, expected-head fences, and Reviewer targeting unchanged.
For Developer PR and CI reads, derive a separate observation target from successful publication/open-PR mutations recorded for the exact live Turn and execution epoch.
Validate the repository/branch resource, ordered expected-head chain, captured publication tip, and PR result identity before using that evidence.
Include durably acknowledged ancestor replays, not arbitrary prior mutations.
When verified preparation has repaired an existing Change Proposal to a publication's result head, validate the acknowledged publication against its own expected head and treat that equal-result evidence as already applied; it must neither reject reads nor move the target backward.
When recovery has already bound a verified publication as the starting scope, exclude historical replays from this derivation so they cannot roll the observation target backward.
Do not use GitHub's current branch tip or process-local publication maps to grant read access.

## Rationale

Changing the token scope in place would blur immutable authority and derived progress and risk changing mutation fencing or Reviewer behavior.
Using only memory would lose successful publication evidence on reconstruction and obscure replay provenance.
The existing durable ledger and replay acknowledgements provide ordered evidence without a new mutable authority table or Agent-selected PR/SHA arguments.

## Consequences

Each Developer publication-bound read snapshots the live ledger and identifies its queried SHA in its result, including empty check collections.
PR reads reject a changed external head rather than following it, while CI reads can still inspect the exact published commit.
Pending or ambiguous mutations grant no additional access; missing ledger evidence fails closed.
Issue reads, handoff lookup, and mutation actions retain their existing scopes.

## Links

- [Work Item 115](https://github.com/jozala/omnigrex/issues/115).
- [MCP capability boundary](../adr/0004-mediate-agent-capabilities-through-mcp.md).
- [Scoped CI diagnostics](0023-scoped-ci-failure-diagnostics.md).
