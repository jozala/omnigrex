# Human-input waiting visibility

Implementation of [Work Item 213](https://github.com/jozala/omnigrex/issues/213), accepted pair P1.
The immutable complete requirements and technical plan remain in Work Item collaboration history.
This delivery plan records implementation boundaries, not a replacement specification or deployment authorization.

## Delivery

1. Add one focused genuine-wait projection and a dedicated durable presentation revision.
   Refresh the observation under the existing Workflow lock at collaboration saves, lifecycle decisions and final settlement of live or corroborated Turns.
   Pending input for the responsible Participant, active Turns and pending preparation/revalidation override historical wait flags.
2. Extend the existing effect context and acknowledgement fences.
   Use label-only jobs for presentation changes and upgrade reconciliation, without invoking Stage mutations or creating progress comments.
   Ordinary Stage jobs also reconcile this independent Issue-only observation before verification deferral or PR-ready validation.
   Forward migration 000009 adds pre-write recovery obligations so a failed late-return correction enqueue cannot strand a stale label after a replacement worker finishes.
   Expired uncertainty uses bounded, throttled label-only probes; only successful probes recur, without resetting failed-job or agent budgets.
3. Seed label-only reconciliation through forward migration 000008 for existing collaboration Workflows.
   Evaluate current records on execution; do not backfill migration-time desired booleans or change histories, budgets, authority or execution allocation.
4. Document the user-visible label and its distinction from CI waiting and Human Handoff.
   The [decision record](../decisions/0069-project-human-input-waiting-independently.md) explains projection and fencing tradeoffs.

## Regression coverage

- Predicate tests cover active Stages, lifecycle exclusions, proposed-pair confirmation without questions, publication provenance, pending input, active/queued continuation and historical ownership.
- GitHub tests cover Issue-only creation/removal, idempotence, unrelated labels and CI overlays, final convergence, preparation deferral and stale PR-ready cleanup.
- PostgreSQL tests cover each Role's genuine settlement, partial answers admitted during remote writes, failed/unknown responses, expired leases and writes finishing after a newer worker, failed return-path persistence, repeated probes and exhausted probe budgets, restarted workers, version changes without Stage changes, continuation ownership, source replay against a restored wait and closure cleanup.
- Existing recovery matrices assert projection for replies before settlement, corroboration, empty yielding, unverified publication and Planner confirmation/completion.
- Upgrade tests apply the real previous migration prefix, preserve collaboration history and lifecycle records, seed one label-only job per record, retain queued preparation and allocate no Turns, including on migration replay.

## Verification boundary

Run focused Go tests, `mise run check`, `mise run docs-check` and Docker-backed `mise run test-integration` before handoff.
Inspect exact-head CI separately; local tests do not establish Linux/KVM runtime qualification.
No production deployment, repair or reactivation is included.
