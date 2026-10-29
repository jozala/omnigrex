---
status: accepted
date: 2026-10-28
decision-makers: [Omnigrex Developer]
---
# Carry current Human Handoff publication across tracking revisions

## Context
Tracking-only Change Proposal synchronization preserves a Human Handoff but advances the Workflow revision.
Revision fencing correctly suppresses or cleans up the older publication job, which can leave labels without an explanation when no replacement publication exists.

## Decision
The reducer explicitly carries the current Human Handoff publication obligation when synchronization advances a `NEEDS_HUMAN` Workflow.
In the same transaction as the revision update, the Store enqueues a normal handoff publication for the new revision, copying the reason and diagnostic from the immediately preceding revision's normal handoff job.
The source must match the preserved reason and exact source revision; safety diagnostics are excluded.
If legacy state has no matching publication job, publish the retained reason without inventing a diagnostic from older history.
Each replacement retains its own operation marker and existing revision, lease, retry, and cleanup fences.
No Agent Turn is scheduled and explicit activation remains necessary to resume work.

## Rationale
A fresh revision-scoped obligation preserves the existing stale-effect protocol, including changes during remote calls and recovery of ambiguous outcomes.
Allowing old jobs to publish merely because state or reason still matches would also authorize replaced blockers with the same reason.
Treating normal handoffs as unfenced safety diagnostics would republish obsolete explanations after resume or closure.
Searching arbitrary historical jobs for a diagnostic would confuse repeated handoffs; only the exact predecessor is carried.
A separate mutable handoff record is unnecessary here because durable job payloads already retain the explanation and the reducer identifies the tracking-only transition explicitly.

## Consequences
Multiple tracking revisions carry the explanation forward transactionally, even if workers have not yet published any revision.
Old jobs still converge through suppression and operation-scoped cleanup, independently of the replacement's publication order.
This does not repair already-lost historical diagnostics on deployment or weaken failure escalation, mutation admission, or explicit-resume policy.

## Links
- [Work Item 156](https://github.com/jozala/omnigrex/issues/156)
- [Tracking without resuming](0022-track-heads-without-resuming-work.md)
- [Visible Workflow and execution state](../adr/0001-separate-visible-workflow-and-execution-state.md)
