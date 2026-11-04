---
status: accepted
date: 2026-11-04
decision-makers: [Omnigrex Developer]
---
# Return durable checkpoint non-publication as an expected result

## Context

[Work Item 175](https://github.com/jozala/omnigrex/issues/175) exposed a valid late amendment as an opaque tool failure and unnecessary Human Handoff.
The input fence, frozen preparation and history bounds remain required by decisions [0055](0055-serialize-work-item-clarification-at-turn-boundaries.md), [0056](0056-bound-collaboration-prompt-projections.md), [0057](0057-fence-retrieval-only-input-on-durable-read-coverage.md) and [0058](0058-retain-paired-planning-checkpoints-in-collaboration.md).
Retained Participants keep older Pi bridges under [0009](0009-pin-participant-images-and-use-working-revision-tools.md).

## Decision

Return an ordinary structured MCP result with `publication: not_applied`, without a revision or source, for authoritative pre-publication rejection.
Distinguish `late_input`, which requires empty-question yield and the orchestrator's next Turn, from `unread_input`, which requires completing frozen retrieval before dependent action.
Persist operation-bound rejection evidence under the same Workflow lock as reservation before returning it.
Exact-operation replay returns the recorded result, not another publication attempt; changed content or a new attempt after delivery requires a new identity.
Keep uncertain database acknowledgments and external publication on exact-operation reconciliation.

Recovery first consults committed rejection evidence or the committed reservation.
A committed reservation still requires exact-artifact reconciliation, never blind reposting.
If neither exists, recovery may prove non-application only under the Workflow lock after durable Runtime Process stop and closed mutation admission for the exact interrupted invocation.
No publication can have started without a committed reservation, and the old invocation can no longer acquire one.
Recovery records that evidence durably before reporting it; when no input fence remains, the reason is `interrupted_before_reservation` and a fresh attempt may use a new identity.
An absent history entry alone never establishes mutation outcome.

## Rationale

Expected successful tool handling is compatible with retained Pi bridges, unlike expanding error-message allowlists or rebinding runtime images.
Durable non-publication evidence closes the lost-result acknowledgment window, while fenced recovery closes the earlier interrupted-reservation window without inventing a publication or leaving a known non-effect stuck indefinitely.
Unlocked preflight checks, widening history, removing input fences and blind reposting would sacrifice ordering or external-effect safety.

## Consequences

Result consumers must inspect publication semantics, not merely tool success.
The retained Agent Session reinterprets delivered amendments and steering before revising the plan; no new Workflow outcome, budget reset, implementation allocation or automatic production reactivation is introduced.
Deterministic qualification proves this mechanism, not real-model reasoning quality.
