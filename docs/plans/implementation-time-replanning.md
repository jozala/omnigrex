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

## Incomplete implementation checkpoint

The current prototype is not a delivered replanning capability and is not ready for review or deployment.
It introduces collaboration request/cycle types and initial eligibility/replay/origin tests, plus reducer contracts for dispatch/resolution and the independent return cap.
The initial domain and reducer tests have passed locally.
Production capability, Store and migration changes are incomplete scaffolding, not verified routing or authority.

The remaining safety-critical work includes atomic dispatch/activation and counter persistence, complete terminal receipts and recovery, immutable cross-Participant delivery and correction fences, exact resumed authorization/admission, revision-bound review/admission/readiness, and controlled retained-role qualification.
In particular, input arriving after a Developer request cannot simply be reparented to the Planner or omitted, and resumption must not reuse initial admission's budget reset.
Do not enable or deploy the prototype as if these boundaries were implemented.
The original Work Item remains the governing completion contract; this checkpoint does not narrow its acceptance criteria.

### Observed verification of the unfinished checkpoint

- `go test -race ./internal/collaboration ./internal/workflow ./internal/role -count=1`: passed after the final prototype edits.
- `mise run docs-check`: passed.
- Compile-only `go test ./internal/mcp ./internal/agentturn -run '^$'`: passed before the last collaboration and Workflow hydration edits; this is not behavioral qualification.
- `mise run check`: failed when the concurrent dead-code task was killed; a serialized retry subsequently exposed existing envelope/tool-inventory/telemetry contract failures caused by the additive prototype, and did not establish a passing gate.
- Docker-backed initial implementation tests were attempted on an intermediate source snapshot and failed Role-policy construction while the prototype was being edited.
- Controlled Docker Pi Workflow was attempted on an intermediate source snapshot and failed production backend construction before meaningful retained-role execution.
- None of these integration attempts qualify the final checkpoint or demonstrate the required material-contradiction scenario.
- No exact-checkpoint CI, Linux/KVM qualification, revised-consent scenario or final native review has been observed.

The failed checks are not acceptance evidence and must not be waived by the focused domain passes.
Complete the integration and repair its contract regressions before requesting review.
