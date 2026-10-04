---
status: accepted
date: 2026-10-04
decision-makers: [Mariusz]
---
# Publish handoff summaries before continuing

## Context

The Developer supplies a summary to `request_review`, including after requested changes, but it has only been retained inside the mutation ledger.
Humans and the Reviewer need a reliable way to see the handoff and associate its claims with the relevant head.

## Decision

Publish each new successful review handoff summary as an idempotent PR comment and expose the latest matching summary through scoped MCP access.
Require confirmed publication before admitting the next Stage.
Use durable reservation identities and reconciliation to handle lost responses without duplicate comments.

## Rationale

Continuing while comment publication retries would make the human-visible collaboration state lag behind autonomous work.
Waiting for confirmation keeps the handoff understandable to humans and gives failures an explicit recovery path.

## Consequences

A GitHub publication outage can delay the next Stage even when the summary is saved internally.
Use bounded recovery and Human Handoff if publication cannot be confirmed.
Scope retrieval to the Workflow, PR, and current head, and treat summary contents as claims to verify rather than approval evidence.
See the [implementation plan](../plans/2026-10-04-instruction-ownership-and-handoffs.md).
