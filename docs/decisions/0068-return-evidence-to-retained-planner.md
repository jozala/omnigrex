---
status: accepted
date: 2026-11-15
decision-makers: [Omnigrex Developer]
---
# Return implementation evidence to the retained Planner

## Context

[Work Item 166](https://github.com/jozala/omnigrex/issues/166) defines evidence-based implementation-time replanning after exact-plan authorization.
The existing lifetime first-Developer admission cannot represent unused consent for a materially revised pair.
Head-only review identity cannot distinguish two plans evaluated against the same commit.

## Decision

Keep requests, immutable evidence and resolution history in the existing locked collaboration aggregate.
Use the existing planning and implementation Stages and Participants, transferring responsibility only after normal Turn settlement barriers.
Count autonomous returns in the Workflow Attempt, once at committed dispatch, with three allowed returns and Human Handoff on the fourth needed return.
Human discussion, plan revision, consent and resumption do not reset this counter or Review Cycles.
An independently requested human material revision can be exempt only through delivered, assessed, immutable steering/amendment evidence.

Preserve the original first-admission marker and add revision-specific resumed bindings rather than recycling initial admission.
Install the new governing binding only when the resumed Developer Turn is durably admitted.
An unchanged resolution retains existing valid authority; a material revision requires a complete linked pair and renewed exact-revision authorization.
Actual withdrawal requires fresh consent even when the documents remain unchanged, without fabricating a material revision.
Scope review readiness and terminal review evidence to accepted revision as well as head.

## Rationale

A second Workflow or replacement Change Proposal would fragment provenance and permit budget resets.
Immediate routing from tool success would race admitted mutations and execution shutdown.
Republishing unchanged documents or requiring renewed approval for a false alarm creates avoidable human work without improving authority.
Treating an old same-head approval as current would omit review of changed commitments.

## Consequences

The change must extend persistence, admission, terminal recovery and bounded evidence delivery together before becoming available.
Checkpoint preservation remains instruction-only through existing publication tools, with no guarantee to restore unpublished edits.
This decision establishes the implementation approach, not a claim of qualification or deployment; see the working plan for observed verification.

## Links

- [Implementation working plan](../plans/implementation-time-replanning.md).
- [Exact-plan initial admission](0067-admit-exact-plan-implementation-at-the-first-developer-turn.md).
- [Clarification boundaries](0055-serialize-work-item-clarification-at-turn-boundaries.md).
- [Bounded projections](0056-bound-collaboration-prompt-projections.md).
- [Complete paired checkpoints](0058-retain-paired-planning-checkpoints-in-collaboration.md).
- [Explicit assessments](0062-use-short-conversation-references-and-explicit-assessments.md).
