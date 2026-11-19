---
status: accepted
date: 2026-11-19
decision-makers: [Omnigrex Developer]
---
# Distinguish handled request rejection from terminal intent

## Context

[Work Item 223](https://github.com/jozala/omnigrex/issues/223) exposed a deterministic immutable-question validation error as uncertain publication.
Unlike resolution, clarification request is a terminal tool: changing an error to an ordinary result alone would make its successful ledger entry compete with a later genuine terminal action.
Decisions [0059](0059-return-durable-checkpoint-nonpublication.md) and [0066](0066-return-definite-resolution-rejections.md) already establish durable non-application and pinned-bridge compatibility.

## Decision

Return a bounded canonical `status: rejected`, `publication: not_applied` result only after the authoritative request-validation transaction commits its operation-bound receipt.
Do not allocate a round, reference or artifact for rejected requests.
For count-only mistakes proven before reservation, commit a fingerprint-bound admission receipt and return the same structural feedback without fabricating a mutation reservation.
A committed receipt preserves exact replay and rejects corrected content under the old caller identity; an ambiguous receipt commit is not non-admission proof.
Advertise Role count limits in the tool description rather than `maxItems` in the discovered schema: Pi validates that schema before calling the server, which would otherwise mask actionable feedback in retained bridges.
Keep item validation and gateway request-byte bounds; direct backend count validation still records receipts for already reserved operations.
A request blocked by an earlier unsettled publication directs exact-operation reconciliation, not a fresh-operation bypass.
A receipt for the new request does not settle the earlier publication.

Exclude only the recognized, well-formed rejection contract from terminal intent, ancestor counts, terminal replay eligibility and successful publication-effect verification.
Keep malformed successful evidence fail-closed rather than skipping all results without waiting evidence.
Recover lost acknowledgments from exact durable receipts; retain stopped-and-fenced non-publication proofs and exact-artifact reconciliation for other outcomes.

## Rationale

Name/state-only selection confuses successful tool handling with Stage completion.
Relaxing question immutability, widening bridge error allowlists, or creating a separate retry/recovery subsystem would undermine existing authority or retained-Session compatibility without solving terminal selection.
A shared strict contract makes the distinction explicit at each ledger consumer.

## Consequences

Agents may correct internal arguments using existing conversation evidence without repeated human answers or per-attempt GitHub comments.
Identical replay remains rejected after state changes, including a retry checkout head change; it restores no publication progress or current-head freshness.
Corrections require a fresh identity only after definite rejection.
This adds no retry budget, authority, successor allocation, deployment action or historical production recovery.
Go recognition and SQL selectors must remain aligned and covered by settlement and recovery tests.
