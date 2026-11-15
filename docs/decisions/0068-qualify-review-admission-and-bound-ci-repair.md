---
status: accepted
date: 2026-11-15
decision-makers: [jozala]
---
# Qualify Reviewer admission and bound automatic CI repair

## Context

Completion-only admission invokes a Reviewer to route already-known failures and admits incomplete evidence at timeout.
[Work Item 195](https://github.com/jozala/omnigrex/issues/195) and its accepted P2 pair replace that policy while retaining durable agent-free waiting and trusted check selection.
This supersedes [decision 0049](0049-gate-reviewer-admission-on-ci-completion.md), not its historical rationale.

## Decision

Admit selected success, neutral, and skipped checks, or legacy status success, without relabeling conclusions.
Route definitive current selected failures directly to the Developer, never as synthetic reviews or Blocking Findings.
Wait for incomplete evidence until the absolute deadline, then create Human Handoff.
Default new requests to two hours; preserve shorter explicit settings, registration grace, frozen inventory, and existing deadlines.
Retain the explicit immediate bypass and positively confirmed no-CI exception.

Allow three committed repair dispatches since reset per Workflow Attempt, independently of actual Review Cycles and infrastructure retries.
Reset only on complete nonempty literal-success evidence accepted at Reviewer allocation.
Use the same Workflow lock, live lease, generation, Participant and Session-control fences for routing, retirement, counters and successor creation.
Record immutable repair/reset decisions so old success cannot erase later repair usage.
Reevaluate legacy released-but-unallocated requests; do not revoke running Turns or reactivate stopped work.
Coordinate rollout with all old workers stopped.

## Rationale

Strict-success-only admission would wrongly call neutral/skipped failures; unlimited repairs would permit autonomous failure loops.
A never-reset total cap was rejected in favor of the requested successful-CI reset, accepting that more than three total repairs may occur in one Attempt.
A second watcher or idle agent would duplicate existing durable coordination and provenance.

## Links

- [Verified CI provenance, 0047](0047-verify-ci-read-provenance.md).
- [Read-target authority, 0038](0038-derive-publication-read-targets-from-the-turn-ledger.md).
- [Participant handoff provenance, 0048](0048-retain-participant-handoff-provenance.md).
- [Code-defined Workflow, ADR 0007](../adr/0007-use-code-defined-workflow-definitions-and-durable-attempt-stages.md).
- [Operator scheduling and rollout](../operators/review-scheduling.md).
