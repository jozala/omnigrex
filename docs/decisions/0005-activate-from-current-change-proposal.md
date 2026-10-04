---
status: accepted
date: 2026-10-04
decision-makers: [Mariusz]
---
# Activate from the current Change Proposal

## Context
Supervising a Work Item from its Change Proposal required navigating back to the Work Item Issue to add `omnigrex:run`, because Pull Request label events were ignored.

## Decision
Accept `omnigrex:run` on the Workflow's current Change Proposal as the same activation command as on its Work Item Issue, reusing Attempt allocation, Stage selection, budget resets, Session Continuation, terminal-intent revalidation, human-control restrictions, active-Turn deferral, and label reconciliation.
Require the durable current Change Proposal association matching repository ID, Pull Request ID, and Pull Request number, resolved under the same transaction and Workflow locking as the transition.
A copied Workflow marker alone is insufficient, historical or unrelated Pull Requests remain non-actionable, and the command never creates a Work Item, adopts an external Pull Request, or reopens a closed Issue.

## Rationale
Reusing the existing Issue-trigger path keeps reactivation, lifecycle, concurrency, idempotency, and visible-label behavior predictable instead of introducing a separate Pull Request state machine.
Constraining activation to the durable active association prevents historical artifacts from controlling current automation while preserving existing conflict rejection for ambiguous evidence.

## Consequences
Accepted commands consume the trigger and converge state labels on both the Issue and the current Pull Request through the existing durable worker.
Rejected commands record a durable non-actionable outcome without label cleanup.
Duplicate deliveries, concurrent Issue and Pull Request commands, and pending or deferred replay remain safe through existing deduplication and fencing.
See the delivery plan in [issue 45](https://github.com/jozala/omnigrex/issues/45); accepted policy is distinct from shipped behavior.
