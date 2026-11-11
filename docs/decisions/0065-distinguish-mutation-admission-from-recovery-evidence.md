---
status: accepted
date: 2026-11-11
decision-makers: [Omnigrex Developer for Work Item 201]
---
# Distinguish mutation admission from recovery evidence

## Context

A terminal handoff correctly rejected because human input arrived after preparation must not prevent an empty-question yield from delivering that input in the retained Agent Session.
Recovery also encounters completed local default-branch observations alongside ambiguous installations and successful external effects.
These states require different evidence, not a blanket successful-ledger exemption.

## Decision

The Store explicitly classifies definite reservation rejection only where it knows admission stopped before reservation allocation or commit.
The Gateway treats that classification as settled for the current call, without clearing an earlier ambiguous call.
Unclassified database errors, including lost commit acknowledgments, remain conservative.
Neither an absent row nor the text of a dependency error establishes definite rejection.

For an exact-epoch successful `refresh_default_branch`, recovery validates the durable result and its repository, execution resource identity, operation-derived reference, and observation fields.
This completes only the historical mutation barrier; it neither installs files nor provides a new current observation.
Unknown or reconciling refreshes retain the interrupted-installation policy.
Successful external effects retain their existing fresh verification barriers and terminal-corroboration delegation.

Diagnostics use the same selector as reconciliation and distinguish unknown/reconciling rows, selected successful effects, and total selected candidates.

## Rationale

Treating every reservation error as ambiguous causes unnecessary Human Handoff, while treating every error as definite could hide an admitted operation.
Rediscovering an external artifact cannot prove a completed local-only observation, and refetching would silently change its identity and freshness.
Skipping all successful effects would weaken intentional publication, comment, and review verification.

## Consequences

Store adapters must preserve the explicit no-reservation classification rather than infer it from sentinel errors alone.
Malformed successful refresh evidence remains blocking.
No production repair, historical ledger rewrite, automatic reactivation, or new mutation replay authority is introduced.

## Links

- [Work Item 201](https://github.com/jozala/omnigrex/issues/201)
- [MCP authority](../adr/0004-mediate-agent-capabilities-through-mcp.md)
- [Default-branch observations](0050-deliver-default-branch-snapshots-as-additive-git-data.md)
- [Turn-boundary clarification](0055-serialize-work-item-clarification-at-turn-boundaries.md)
