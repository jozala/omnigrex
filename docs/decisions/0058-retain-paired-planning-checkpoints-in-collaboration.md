---
status: accepted
date: 2026-11-03
decision-makers: [Omnigrex Developer]
---
# Retain paired planning checkpoints in the collaboration aggregate

## Context

[Work Item 164](https://github.com/jozala/omnigrex/issues/164) requires lightweight immutable planning revisions and explicit confirmation without implementation authorization.
Accepted replies, steering, preparation and Stage handoff already serialize on the Workflow row through [decision 0055](0055-serialize-work-item-clarification-at-turn-boundaries.md).

## Decision

Extend that aggregate with one captured original request, complete paired revisions, publication sources, supersession and evidence-based revalidation records.
Reserve the pair before external publication and recover only an exact operation-marked artifact through the shared channel-neutral publication boundary.
Expose planning history through the existing bounded collaboration projection/retrieval mechanism.
Reuse immutable human-input admission and safe Turn boundaries for exact-revision confirmation; a receipt does not finish planning.
Commit successful readiness only at fenced settlement, not tool execution, because input can arrive between those events.
Select Profiles for the Stage being prepared so optional Planner configuration does not become a development prerequisite.

## Rationale

Separate document repositories or independent brief/plan versions would add coordination and partial-acceptance states without serving this increment.
Deriving authority from mutable Issue comments would lose accepted contents and make retries unsafe.
Completing planning in the comment handler would skip input delivery and repository relevance assessment.
Requiring all Profiles up front would break direct development.

## Consequences

Both outputs change under one revision identity and material revisions require renewed confirmation.
Long histories reuse bounded reads; comments are readable views rather than the authoritative model.
Presence validation does not prove semantic completeness, and controlled orchestration does not replace the required one-time human assessment.
There is no planning-to-development transition, new isolation regime or additional provider authority.

## Links

[Implementation plan and verification status](../plans/planning-only-planner.md).
