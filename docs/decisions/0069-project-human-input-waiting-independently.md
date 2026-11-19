---
status: accepted
date: 2026-11-19
decision-makers: [Omnigrex Developer]
---
# Project human-input waiting independently of Stage

## Context

[Work Item 213](https://github.com/jozala/omnigrex/issues/213) needs an Issue-scanning indication of ordinary clarification and exact-plan-confirmation waiting.
The accepted pair P1 requires visibility without changing collaboration authority or continuation semantics.
A settled clarification flag can remain true after accepted input queues continuation, and an external label write can overlap that admission.

## Decision

Project `omnigrex:waiting-for-input` only on the Work Item Issue, outside mutually exclusive Stage labels and independently of CI waiting.
Compute genuine waiting under the Workflow lock from lifecycle, active Attempt and responsible Participant, settled collaboration waiting, verified question or proposed-pair publication, pending input and continuation activity.
Keep the existing scheduling flags unchanged.

Cache the observation solely to detect changes, with a dedicated monotonic presentation revision and a label-only job inserted in the same transaction.
Refresh at collaboration saves, persisted lifecycle decisions and final Turn settlement, after active execution has ended.
Workers recompute current state rather than consuming a payload boolean, and acknowledgement fences include the presentation revision.
Superseded effects retain durable corrective work even when the remote response reported failure.
If an external request returns after its lease is lost, historical attempt provenance supports one idempotent label-only correction after that request, even if a newer worker already finished.
Migration seeds the ordinary serialized label worker for existing collaboration records, including terminal cleanup, without allocating Agent Turns.
Label-only jobs bypass Stage effects; ordinary jobs reconcile the overlay before preparation deferral or PR-ready validation.

## Rationale

Using `needs-human` or a new Workflow state would misrepresent ordinary discussion.
Projecting the raw wait flag or removing labels in command handlers would miss queued continuations, settlement races and restart recovery.
An unversioned second GitHub writer would allow obsolete additions to overwrite newer removals without fenced correction.
Reusing existing leased effects preserves bounded retries and failure policy.

## Consequences

Accuracy is eventual after external effects settle, not a distributed GitHub/PostgreSQL transaction.
Publication or retry failures remain subject to existing operational recovery rather than spending agent or review budgets merely to refresh labels.
New collaboration and lifecycle persistence paths must preserve the presentation refresh boundary.

## Links

- [Implementation plan](../plans/human-input-waiting.md).
- [Clarification serialization](0055-serialize-work-item-clarification-at-turn-boundaries.md).
- [Paired planning checkpoints](0058-retain-paired-planning-checkpoints-in-collaboration.md).
- [Exact implementation admission](0067-admit-exact-plan-implementation-at-the-first-developer-turn.md).
- [Visible Workflow and execution state](../adr/0001-separate-visible-workflow-and-execution-state.md).
