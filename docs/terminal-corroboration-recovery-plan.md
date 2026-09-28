# Terminal-intent corroboration recovery — working implementation plan

Status: **implemented in the working tree; not deployed or release-qualified**. This is a handoff plan, not an
accepted specification or an operator recovery procedure. Follow [AGENTS.md](../AGENTS.md),
[CONTEXT.md](../CONTEXT.md), the relevant [ADRs](adr/), and the
[operator guide](operator-guide.md) before changing code. Do not edit
`docs/first-iteration.md` or manually repair PostgreSQL or runtime-state files.

## Problem and observed case

Workflow `e5261bb5-395d-401a-8b19-e42b7ef6e23f` (Issue #15, PR #16) had a
Developer Agent Turn `c4825fc1-adeb-41f2-b8cc-d89a0cc9638d` whose MCP
`request_review` mutation **succeeded** at head
`b26793155afbcb4499ea91dd248d986505a519e9`. Its ACP prompt ended normally.
The orchestrator's fresh `GetPullRequest` corroboration failed and the Turn was
atomically settled as `INFRASTRUCTURE_FAILED`. A new retry Turn
`45b3f440-ede4-4feb-8e7c-0e2951536dc0` saw the prior successful action,
concluded there was nothing to do, and ended without a terminal mutation. Its
settlement exhausted the Workflow Attempt's infrastructure retry budget.

The agent's final text is **not** transition authority. A successful terminal
mutation, an eligible ACP prompt outcome, and fresh corroboration are required
before the Workflow can advance. The failed GitHub read was in the orchestrator,
not an agent OOM; those two Turns have `end_turn` outcomes and no recorded OOM.
The underlying GitHub read error was not retained by the old settlement.

Pre-change behavior (the regression): `internal/agentturn/outcome_reconciler.go` converted
Developer `GetPullRequest` and Reviewer `GetPullRequest` /
`ListPullRequestReviews` errors to an infrastructure-failure observation.
`internal/agentturn/execution_worker.go` settled that observation immediately;
its bounded finalization retry applied to returned **errors**, not to that
observation. `internal/workflow/reducer.go` created a **new Agent Turn** for an
infrastructure retry. `internal/store/turns.go` lists terminal mutations for
the exact Turn/epoch; retry Turns do not implicitly inherit that ledger.
`internal/store/outcome_queries_integration_test.go` tests that a retry without
explicit replay has no terminal intent. The original settlement is immutable.

## Decisions already agreed

- Cover Developer `request_review` and Reviewer `submit_review`, not only the
  observed Developer case. Preserve existing handling for `report_blocked`.
- Once a terminal mutation succeeded and the ACP result is **known and
  eligible**, keep the original Turn's outcome pending during unavailable
  corroboration. Verify it without a new agent prompt or repeated external
  mutation. The Runtime Process must stop and global execution capacity be
  released, but Workflow and Agent Session authority remain fenced.
- Retry all **retryable corroboration reads**, including GitHub, database, and
  local workspace observations. Do not treat a definite identity/head/tree
  conflict as temporary unavailability. Preserve the role-specific stale-head,
  synchronization, and publication-conflict behavior where safe.
- Use a durable, configurable corroboration window, default **30 minutes**,
  with a code-defined immediate first check then exponential delays starting
  around **5 seconds**, capped around **2 minutes**. Persist `pending_since`,
  not the configured duration. Evaluate `pending_since + current configuration`
  when work wakes: an unchanged restart does not reset the window; a changed
  configuration may move the deadline. Applying a shortened window on the next
  scheduled wake is acceptable.
- Treat network failures, timeouts, rate limits, 5xx, invalid API responses,
  and a potentially not-yet-visible PR (404) as retryable. A 401 or
  non-rate-limit 403 causes an immediate Human Handoff. A later human-triggered
  attempt may revalidate the original intent after the access issue is fixed.
- On window expiry, create Human Handoff with a **distinct reason** such as
  `terminal_corroboration_exhausted`, a safe last failure category, and an
  explanation that the terminal mutation succeeded but its Workflow outcome
  could not be corroborated. Do not start another agent solely because this
  verification window expired. Use a distinct prerequisite diagnostic for
  immediate permanent access failure. Keep the existing developing/reviewing
  GitHub label while verification is pending; add no new label.
- Issue closure preempts verification and must make later advancement
  impossible. No new manual cancellation command is required. A pending Turn
  must prohibit another prompt and human Agent Session control transfer.
- Preserve current ACP eligibility: normal completion, or the currently
  accepted lost-response/deadline cases, may settle a successful terminal
  mutation. Explicit cancellation, refusal, and other non-normal stop reasons
  cannot become success merely because a mutation succeeded.
- **Unknown ACP ending after a crash is not eligible for automatic success.**
  The mutation ledger alone cannot prove the absence of cancellation. Use the
  existing recovery/new-Turn direction, but supply explicit prior-intent
  context. Add a narrowly scoped MCP `confirm-prior-terminal-intent` operation:
  the agent chooses to confirm the prior successful mutation, Omnigrex checks
  lineage, authority and fresh state, and the new Turn obtains terminal
  evidence **without repeating a GitHub review**.
- After corroboration exhaustion or an immediate access-related handoff, a
  human's later `omnigrex:run` creates a **new Workflow Attempt** and first
  revalidates the old eligible intent, without an agent prompt. This gets a new
  30-minute window if GitHub remains unavailable. Add an explicit,
  provenance-checked transition under the **new Attempt**, not a rewrite of
  the old Turn's settlement or a synthetic replay into a new Turn. If the
  evidence is stale, use a safe role-specific continuation where one exists;
  otherwise hand off. If original Developer workspace/publication tree proof
  is missing, do not infer readiness from the remote PR alone. A newly accepted
  changes-requesting review counts as **one** Review Cycle in the new Attempt;
  a duplicate already-accepted review counts zero.
- Apply this feature only to **future** Turns/checkpoints. Do not retrofit or
  rewrite the recorded settlements of Workflow
  `e5261bb5-395d-401a-8b19-e42b7ef6e23f`.

## Safety constraints and implementation seams

1. **Checkpoint before losing prompt evidence.** After admission closes and
   mutations drain, persist the ACP classification/stop reason and exact
   terminal mutation provenance under a fenced live Turn. Do not infer a
   missing prompt result from a successful mutation. Ensure a crash between
   checkpoint, Runtime Process cleanup, and worker handoff enters a safe stop
   barrier; never corroborate against a still-running process. A crash before
   checkpoint follows the unknown-ending path above.
2. **Durable pending authority.** Add a forward-only migration (the current
   highest migration is `000025`; choose the next available number at
   implementation time). Model a pending original Turn/checkpoint and one
   idempotent, delayed verification job. The job needs a lease and database
   attempt history; expired job leases and response-loss replay must not fall
   through to the existing generic infrastructure-retry transition.
   `agent_turn_slots` capacity can be released while retaining an explicit
   Workflow/Session fence. Update startup recovery, the periodic expiry
   monitor, control transfer, and closure accordingly.
3. **Read classification without unsafe errors.** Give the outcome reconciler
   a typed distinction between retryable **unavailable observation**,
   permanent prerequisite failure, and definite fresh evidence mismatch.
   Do not persist or log raw GitHub errors, response bodies, repository
   contents, or credentials. Reacquire the appropriate Role's installation
   credential on each delayed check. Retryable Store failures must not be
   mistaken for a conclusive absence of evidence.
4. **Original-Turn settlement under worker authority.** Refactor
   `internal/store/settlement.go` so a verification worker can apply the
   existing reducer's successful `TurnSettledEvent` (or role-specific
   definite outcome) against the original Turn only after stop, mutation, job
   lease, Stage, head, and Workflow revision guards all pass. Atomically
   persist settlement provenance, Review/Change Proposal identity, successor
   jobs, label effects, and completion. Do not let the same original intent
   enqueue two Reviewer Turns or consume two Review Cycles. Existing recovery
   currently hardcodes `INFRASTRUCTURE_FAILED`; merely returning a new error
   from `Reconcile` would still create another agent retry after recovery.
5. **Fresh Developer and Reviewer proof.** Preserve the checks in
   `outcome_reconciler.go`: exact PR identity and head, open/unmerged state,
   publication marker where required, and Developer normalized
   workspace/publication-tree equality; for Reviewer, exact review ID,
   node/state/commit/actor and authorized Reviewer App. A newer PR head may
   produce Reviewer stale-head synchronization without accepting an approval
   for the wrong head. Keep the Developer tree protected until verification
   completes; workspace preparation for a next Turn can otherwise replace it.
6. **New-Attempt revalidation is a distinct transition.** Today's `TriggerEvent`
   immediately enqueues `PREPARE_AGENT_TURN`, and `TurnSettledEvent` requires
   an active Turn belonging to the same Attempt. Gate preparation when the
   new Attempt has an eligible prior terminal intent; create an explicit
   revalidation job and reducer event with source Turn/settlement/mutation,
   same Participant and Stage/Role, expected head, prompt eligibility, and
   current Attempt revision guards. Do not fabricate a current-Turn mutation.
   Check existing accepted review identity before applying a review outcome.
   Process/defer concurrent webhook head changes and Issue closure under the
   same fences.
7. **Unknown-ending agent confirmation is separate.** Ordinary MCP exact
   replay is scoped to `operation_lineage_id` and an ancestor `retry_of_turn_id`;
   a new Workflow Attempt has a fresh lineage. Within the eligible retry
   lineage, the explicit confirmation tool must verify the source success,
   turn/participant/attempt binding, unchanged relevant head and review
   identity, and current Agent Turn authority; it must record a fresh terminal
   intent without calling GitHub `SubmitReview` again. If evidence is stale or
   the agent does not confirm, do not advance automatically.

## Suggested implementation order

Keep each step guarded so a partially built feature cannot advance a Workflow:

1. Add regression fixtures for successful terminal mutation + unavailable
   corroboration, Developer and Reviewer, and for a successor agent that
   produces no terminal intent. Add typed observation classification.
2. Add the checkpoint/schema and fenced transition to pending verification,
   including Runtime Process stop and capacity release. Add restart and lease
   expiry tests before enabling the worker in production.
3. Add the verification worker, current-configuration deadline/backoff,
   atomic original-Turn settlement and exhaustion/prerequisite handoffs.
4. Add closure/control/webhook races and safe diagnostics; prove no duplicate
   GitHub effect or successor on ambiguous response.
5. Add the provenance-checked new-Attempt revalidation event/job, replacing
   immediate Turn preparation only for eligible recorded sources.
6. Add the agent-facing prior-intent notice and fenced confirmation MCP tool
   for unknown ACP endings. Cover Developer and Reviewer nonduplication.
7. Wire configuration through `internal/config`, `cmd/omnigrex`, local and
   deploy Compose files and environment examples, and document behavior in
   the [operator guide](operator-guide.md) without copying its procedures.

## Verification matrix

- Unit tests: typed read classifications (including 401, non-rate-limit 403,
  404, 429, 5xx, invalid response, DB/workspace outage), ACP eligibility,
  backoff/deadline calculation, original/new Attempt reducer decisions,
  duplicate/contradictory intent rejection, sanitized logs/diagnostics.
- Docker-backed Store integration: pending checkpoint and slot release;
  worker lease expiry and restart; ambiguous settlement commit and duplicate
  job delivery; Issue closure race; PR synchronize/head replacement; Developer
  tree loss; Reviewer review identity and budget exactly once; reactivation
  after expiry and after fixed App access; unknown ACP ending versus recorded
  cancellation; same-lineage confirmation without a second GitHub review.
- Follow [README.md](../README.md) and `mise.toml`: run the narrowest tests,
  `mise run check` for every code change, and `mise run test-integration` for
  Docker-backed Store behavior. `mise run test-compose` recreates stable
  volumes/networks and must not run without clearance that those resources
  may be removed.

## Current implementation and local baseline

The uncommitted working tree now includes migration `000026`, a fenced
`CORROBORATING` Turn state, the live verifier and its backoff/expiry path,
closure cancellation, new-Attempt revalidation with internal-event provenance,
the explicit confirmation MCP tool, Role policies, configuration, and focused
Developer/Reviewer Docker-backed Store tests. The short same-Turn GitHub retry
and credential-safe failure-category logs remain. Upstream request IDs are
omitted from outcome logs. None of these changes retroactively rewrites the
already-settled #15 Workflow. Inspect the diff and run the gates before
calling this deployable; do not mistake this working-plan status for a release
qualification.

`mise run check` currently fails in two unrelated baseline areas:
`TestDeployComposeMatchesDevelopmentTopology` because the pre-existing
development Compose networks/labels differ from deploy Compose, and
`TestLifecyclePublishesNormalizedWorkspaceFromCleanCheckout` because local Git
prints `+00:00` instead of the expected `Z`. Focused Agent Turn and Docker-backed
Store integration tests pass, including a populated v25→v26 migration. The
`test-integration` now serializes packages and uses a 30-minute Go timeout;
earlier full Store race runs exposed unrelated timing-sensitive legacy tests
with 30–80 ms leases, which now use enough live-lease headroom before
deliberately expiring. The latest full Store race suite passed, as did focused
race-enabled integration tests for the later replay, cancellation, migration,
and revalidation changes. `mise run test-integration` still fails only on the
pre-existing development/deploy Compose topology mismatch.
Do not run the destructive `test-compose` task without clearance.

The working tree also contains unrelated pre-existing `compose.yaml` network
and label changes, an `opencode.jsonc` file, and an untracked runtime-profile
compatibility artifact. Keep those separate from this feature; do not include
credentials or compatibility metadata in a commit.
