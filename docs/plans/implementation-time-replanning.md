# Implementation-time replanning delivery

Delivery target: [Work Item 166](https://github.com/jozala/omnigrex/issues/166).
This is an implementation working plan, not evidence of delivered behavior.
The Work Item contains the governing product rules and complete source-informed design.

## Boundaries

Reuse the collaboration aggregate and the existing planning, implementation and review Stage identities.
A Developer request records evidence but does not transfer Session control before fenced settlement.
Only an already-authorized plan and existing Planner qualify.
Preserve the original first-admission marker, published commits, Change Proposal association and Review Cycle usage.
Do not introduce automatic checkpoint publication or unpublished-workspace restoration.

## Sequence

1. Add domain contracts and behavior tests for operation-bound requests, single active cycle, source provenance, unchanged resolution and revised authority.
2. Add forward persistence and locked settlement/admission transitions, including once-only autonomous charging and the three-return cap.
3. Wire Developer capability, exact terminal receipts and interrupted-operation reconciliation.
4. Continue the retained Planner with bounded exact governing documents and immutable cross-Participant evidence scope.
5. Extend exact-pair commands and consume resumed authority only at Developer admission without initial budget resets.
6. Bind review readiness, admission and terminal evidence to the accepted revision as well as head SHA.
7. Update instructions, human-facing milestones and current documentation.
8. Qualify controlled retained-role execution, real PostgreSQL recovery/concurrency and normal local/integration gates; inspect exact-source backend CI.

## Verification

Exercise false alarms and material contradictions independently.
Test duplicate and lost responses, closure, delayed input, withdrawal, restart, concurrent admission, exhausted budgets and same-head obsolete reviews at their durable boundaries.
Use the existing controlled Pi fixture for a realistic contradiction through human decision, revised authorization and the same Change Proposal's fresh native review.
Record human effort, rework, decision fidelity and loop behavior without adding accounting machinery.
Local lack of KVM is not a passing backend result; use exact-source CI for that qualification.

## Implemented boundaries

The implementation uses operation-bound request and resolution receipts in the locked collaboration aggregate.
Normal settlement dispatches the retained Planner only after the existing admitted-mutation, execution-stop and capture barriers.
The dispatch transaction records the Attempt and once-only autonomous charge; a fourth pending return retains evidence for explicit Human Handoff recovery.
Independent human-origin classification requires delivered, assessed steering/amendment evidence that has not already been considered in another cycle.

Planner access to Developer-owned evidence is explicit cycle scope, not reparenting.
Frozen bounded records provide complete governing documents and concern/resolution evidence; the accepted pair embeds the original request and does not require duplicate delivery of that body.
Historical correction fences remain in force across transfer.
Initial first-Developer admission remains immutable, with append-only binding history for resumption and no budget initialization on resumed work.
Material revisions require fresh exact consent and revision-bound native review, including at an unchanged code SHA.
Unchanged commitments use still-valid authority; actual withdrawal requires fresh validated consent to the same pair, not invented replacement documents or restoration of the old grant.

## Behavior evidence

- Domain/reducer tests cover scoped eligibility and provenance, independent human origin, duplicate requests, three dispatches/fourth Human Handoff, immutable transferred input and historical correction fences.
- Resumption tests cover material revision, confirmation-only waiting, withdrawal, fresh same-pair consent, source ordering and historical reassessment without revoking newer consent or restoring old authority.
- PostgreSQL tests cover unchanged settlement/admission with retained Participants/Sessions and existing budgets, fourth-return explicit recovery, pre-admission Human Handoff withdrawal, operation-content mismatch and contradictory post-request mutations.
- Lost-response recovery covers both request and resolution effects committed without acknowledgments: mutation reconciliation alone cannot transfer responsibility, and stopped-Turn corroboration settles each original effect once.
- Revision-aware review tests reject historical same-head approvals for a new pair and preserve the remaining Review Cycle limit.
- The forward migration backfills only established initial bindings, preserves direct-development null bindings, history, readiness, stopped state and existing usage, and creates no cycles or grants.

### Controlled evaluation

The deterministic Pi fixtures model a discovered restart-persistence contradiction in an isolated local decision-record design.
The material scenario uses five human commands, including the product decision, confirmation-only waiting and exact revised-pair implementation consent.
The unchanged scenario uses the original two planning/authorization commands and needs no additional approval or duplicate pair.
Each scenario dispatches one autonomous return, retains the same Change Proposal and published partial history, continues compatible retained Role Sessions, and reaches fresh native review.
Avoidable rework is confined to the disposable restart model rather than a production implementation.
The executable model uses separate Node processes to test restart, reset and identifier exclusion.
The original transient implementation is a failing control for the persistent requirement; the revised implementation must satisfy it.
Reviewer qualification also checks that the partial published head remains an ancestor, and failed executable checks prevent scripted publication or approval.
The assertions verify explicit decision fidelity, exact governing revision, authority, preserved history/budgets and final review.
They do not measure real-model reasoning quality or establish deployment qualification.

## Verification status

Focused race-enabled domain, reducer, Store lost-response/unchanged/admission and forward-migration tests have passed during implementation.
Both controlled replanning Pi scenarios passed with the strengthened executable restart/reset model, partial-head ancestry and failed-check guards.
An intermediate serialized `mise run check` passed.
Completion requires the final relevant-source check, complete Docker integration, full controlled Workflow, recovery, upgrade and protocol qualification; the Change Proposal handoff records their final results and any remaining limits.
Exact-head CI, including Linux/KVM qualification, must be inspected separately and is not inferred from local Docker success.
No deployment or live historical Session evaluation is authorized or claimed.

The earlier unfinished prototype was preserved in commit `185719a` and accurately reported as unqualified.
That checkpoint's failed/incomplete checks are not acceptance evidence for this continuation.
