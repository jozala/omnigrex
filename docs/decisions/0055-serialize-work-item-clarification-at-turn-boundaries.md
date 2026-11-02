---
status: accepted
date: 2026-11-01
decision-makers: [Omnigrex Developer]
---
# Serialize Work Item clarification at Turn boundaries

## Context

[Work Item 163](https://github.com/jozala/omnigrex/issues/163) requires normal clarification without changing Workflow Attempt identity, Session ownership, or retry/review budgets.
Question publication and human replies can race with each other, Turn settlement, and successful Stage handoff.
GitHub is the initial interface, not the domain identity or the Session controller.

## Decision

Keep transport-independent rounds, individual questions, immutable accepted input, explicit interpretations, and source-correlated receipts in a focused collaboration aggregate.
Persist that aggregate under the Workflow row lock, the same serialization boundary used for preparation and settlement.
Store the aggregate as versioned-migration-owned JSON rather than exposing a generic messaging API or deriving state from mutable Issue comments.
Domain round and response identities are independent of GitHub artifact identities, which remain source metadata.

Reserve a round before publication and admit explicitly addressed answers even before publication confirmation.
Only a verified publication supports expected clarification waiting; an uncertain effect uses the existing operation-marked mutation reconciliation path.
Expected waiting is a successful Turn settlement in the same Stage, with no autonomous successor until input is available.
Preparation freezes an ordered input batch, and subsequent input fences successful handoff until another safe Turn boundary.
The controller alone allocates continuation of the retained Session.

## Rationale

Using Human Handoff for ordinary questions would require explicit reactivation and reset Attempt budgets.
Prompting a running Session directly would violate [ADR 0003](../adr/0003-orchestrator-owns-session-control.md).
Inferring answers from arbitrary comments or consulting edited source comments would make provenance and replay unsafe.
A separate transport-owned queue would introduce another ordering authority competing with Stage settlement.

## Consequences

Collaboration operations must preserve earlier records and validate current lifecycle and responsibility under the Workflow lock.
Adapters verify actor authorization and source identity; they cannot independently prompt a Session or authorize implementation.
The JSON aggregate favors a small, auditable application boundary over independently queryable normalized message tables; future indexing or retention changes require an explicit migration and preservation of accepted history.
Accepted does not establish release qualification; delivery and verification remain part of Work Item 163.
