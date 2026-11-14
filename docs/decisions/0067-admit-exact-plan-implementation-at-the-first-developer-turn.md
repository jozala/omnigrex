---
status: accepted
date: 2026-11-14
decision-makers: [Omnigrex Developer]
---
# Admit exact-plan implementation at the first Developer Turn

## Context

[Work Item 165](https://github.com/jozala/omnigrex/issues/165) distinguishes pair confirmation, implementation intent, queued preparation and started work.
Withdrawal, human revision, closure, restart and concurrent preparation must have one durable outcome without duplicating implementation or resetting its budgets repeatedly.
[Decision 0058](0058-retain-paired-planning-checkpoints-in-collaboration.md) already retains immutable complete pairs and final relevance validation in the collaboration aggregate.

## Decision

Retain implementation requests and withdrawals in that same locked Work Item aggregate, with actor/source provenance and permanent source deduplication.
An implementation request is also exact-pair confirmation input, but confirmation alone remains planning-only.
Route all requests through Planner assessment and current repository validation, including requests for pre-existing ready pairs.
After validation, queue normal Developer preparation; consume authority and reset implementation's normal retry/review allowance only in the transaction that inserts the first Developer Turn.
Record that Turn, authorization and accepted revision permanently.

Before that transaction, new human input reopens the existing Planner discussion and fences queued or leased preparation.
Repository movement likewise returns to relevance validation while preserving unused authorization; it is not automatically a material change.
Explicit material assessment invalidates authority permanently; withdrawal does not invalidate pair confirmation.
Fresh external actor-permission and open-Issue observations are bound to the exact grant and checked again under the admission lock.

Deliver the exact accepted pair, original request and selected source decisions through the bounded collaboration projection, with lossless supporting history available.
If the complete accepted record is omitted, terminal admission requires successful canonical read coverage of every chunk.
Publish concise milestone receipts through a separate durable collaboration outbox, not label updates or per-retry comments.

## Rationale

A GitHub-only command transition would bypass publication reconciliation, accepted inputs and repository relevance validation.
Treating queued preparation as started would make withdrawal misleading while execution is not admitted.
A second Workflow or Issue would fragment provenance and allow budget resets through replay.
Time-based expiry and blanket commit equality would require unnecessary reapproval instead of evidence-based relevance assessment.

## Consequences

The existing direct-development and normal Developer/Reviewer machinery remain unchanged after first admission.
Unknown publication results retain exact-artifact reconciliation; upgrading existing ready plans grants no authority.
Material implementation-time contradictions require Human Handoff until automatic replanning is delivered.
Operational completed-plan projection recovery remains separate from human-requested revision.
