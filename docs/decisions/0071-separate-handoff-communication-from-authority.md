---
status: accepted
date: 2026-11-27
decision-makers: [jozala]
---
# Separate Human Handoff communication from authority

## Context

A flattened diagnostic cannot reliably communicate human intervention and material uncertainty while keeping supporting evidence out of the main comment.
Allowing agent prose to define continuation would confuse a reported recommendation with the Orchestrator's Workflow authority.

## Decision

Use a small optional communication context in the existing blocked terminal intent, settlement and durable publication job.
The agent reports explanation, intervention, observations, completed work, uncertainty and flexible evidence.
The Orchestrator renders authoritative artifact association and continuation separately, with source observation heads retained across tracking-only changes.
No transition or qualification policy reads context prose.

Preserve lightweight safe Markdown only in this bounded communication path; keep the diagnostic sanitizer unchanged.
Only the Orchestrator owns expandable HTML and operation markers.
Retain minimal missing-context fallback and deterministic recorded-operation recovery, not version negotiation or a compatibility framework.

## Rationale

A single unrestricted string hides required actions among diagnostics, while a generic report schema or executable recovery language introduces unnecessary policy and compatibility surface.
Existing durable jobs already provide revision fences, retry reconciliation and stale-effect cleanup.
An additive optional value reuses those guarantees without a mutable handoff table or historical rewrite.

## Links

- [Work Item 235](https://github.com/jozala/omnigrex/issues/235), accepted pair P1 and decision response A1.
- [Implementation notes](../plans/action-first-human-handoffs.md).
- [Explicit activation](0021-activate-from-current-change-proposal.md).
- [Tracking without resuming](0022-track-heads-without-resuming-work.md).
- [Exact predecessor carry-forward](0052-carry-current-handoff-publication-across-tracking-revisions.md).
