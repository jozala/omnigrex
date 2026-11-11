---
status: accepted
date: 2026-11-10
decision-makers: [Omnigrex Developer]
---
# Return definite resolution rejection as an expected result

## Context

Work Item 197 exposed invalid mixed-round evidence as an opaque Pi tool failure despite a sufficient current human answer.
Retained Participants use pinned bridges which mask tool errors but pass ordinary result content, as described in [0059](0059-return-durable-checkpoint-nonpublication.md).

## Decision

Return structured `recorded: false`, `application: not_applied` results for domain resolution validation rejection.
Keep exact question scope and correction rules unchanged.
Expose canonical structural reason codes and one-based submitted batch/evidence positions, never raw human text or unrestricted dependency errors.
Persist operation-specific rejection evidence under the same locked transaction as validation, before returning definite non-application.
A failed or ambiguous commit remains unknown, even when validation rejected in memory.
Recovery consults the exact operation receipt; absence is not proof of rejection.
Replay returns the recorded result; corrected arguments require a fresh identity only after definite rejection.

## Rationale

An expanded error allowlist would not fix retained bridges without rebinding their images.
Ordinary results preserve compatibility while requiring consumers to inspect resolution semantics rather than transport success.
A durable rejection receipt closes the lost-acknowledgment window without manufacturing resolution origin evidence or weakening historical correction fences.

## Historical proposal prerequisites

Extend the existing [checkpoint non-publication contract](0059-return-durable-checkpoint-nonpublication.md) to stale effective historical interpretations.
Persist structural round/question/evidence/correction references in the exact-operation rejection receipt under the reservation transaction; expose no source bodies or interpretation text.
A correction to an input may stale several interpretations, including substantively unchanged decisions outside the current round.
The agent must explicitly reconcile all affected interpretations against existing evidence; application code must not infer unchanged meaning or remove the historical fence.
Current-round resolution and complete input assessments are not substitutes for those interpretations.
Exact replay stays rejected after reconciliation; a fresh complete pair uses a new identity only after definite rejection.
Ambiguous commits remain unknown and require exact-operation recovery.

## Consequences

Developer, Reviewer and Planner share the same nonterminal result contract.
Agents can correct internal evidence selection without requesting another valid human answer or allocating implementation work.
No automatic production recovery, ledger repair or new Workflow transition is authorized.

## Links

- [Work Item 197](https://github.com/jozala/omnigrex/issues/197).
