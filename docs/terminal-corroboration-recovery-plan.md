# Terminal-intent corroboration recovery — implementation handoff

Status: **Implemented on `fix/terminal-intent-corroboration`, not deployed or release-qualified.**
This file records the design and verification boundaries for a future agent, not an accepted specification or an operator recovery procedure.
Follow [AGENTS.md](../AGENTS.md), [CONTEXT.md](../CONTEXT.md), the relevant [ADRs](adr/), and the [operator guide](operator-guide.md).
Do not edit `docs/first-iteration.md` or manually repair PostgreSQL or runtime-state files.

## Observed failure and intended outcome

Workflow `e5261bb5-395d-401a-8b19-e42b7ef6e23f` (Issue #15, PR #16) had Developer Turn `c4825fc1-adeb-41f2-b8cc-d89a0cc9638d`, whose MCP `request_review` mutation succeeded at head `b26793155afbcb4499ea91dd248d986505a519e9` before the ACP prompt ended normally.
The orchestrator's fresh Pull Request read failed, so the old code settled that Turn as `INFRASTRUCTURE_FAILED` and prompted a retry.
The retry Turn `45b3f440-ede4-4feb-8e7c-0e2951536dc0` remembered the previous action, ended without a terminal mutation, and exhausted the infrastructure budget.
Agent final text is not Workflow transition authority; a successful terminal mutation, an eligible ACP ending, and fresh corroboration are required.
This change applies to future Turns and does not rewrite the historical settlement for Issue #15.

## Agreed behavior

- Cover Developer `request_review` and Reviewer `submit_review`, while retaining the existing `report_blocked` outcome.
- After an eligible ACP ending and a successful terminal mutation, keep the original Turn pending while temporary GitHub, database, or workspace reads are unavailable.
- Stop the Runtime Process and release global Turn capacity before delayed verification, but keep Workflow and Agent Session control fenced against another prompt.
- Retry with persisted, exponentially delayed jobs from about five seconds up to two minutes within a configurable 30-minute default window.
- Persist the start time rather than the configured duration, so an unchanged orchestrator restart does not reset the window and a later configuration change can move its deadline.
- Retry transport failures, deadlines, rate limits, 5xx responses, invalid API responses, a potentially transient Pull Request 404, and a newly submitted review that is not yet visible.
- Preserve role-specific handling for definite PR/review conflicts and stale Reviewer heads; never accept a review or handoff for an unproven head.
- Create an immediate prerequisite Human Handoff for a definite authentication or permission failure, and use the distinct `terminal_corroboration_exhausted` reason when the verification window expires.
- Keep existing developing/reviewing labels while corroboration is pending, and explain successful-but-unaccepted mutations through safe failure categories in the Human Handoff.
- Let Issue closure preempt pending verification and prevent a later Reviewer transition.
- Reject explicit ACP cancellation, refusal, invalid stop reasons, and other non-normal endings as authority for accepting an old terminal intent.
- Treat a missing durable ACP ending after a crash as unknown, not automatically successful; a successor Agent Turn may explicitly confirm a proven prior mutation under fresh fenced authority.
- Do not submit a duplicate GitHub review when confirming, replaying, or retrying a previously successful native review.
- After corroboration-related Human Handoff, a human `omnigrex:run` may open a new Attempt that revalidates the prior intent without a prompt when its Stage, Role, Assignment Generation, and automation control still match.
- Preserve the old failed settlement, record new-Attempt revalidation through an internal event, and count an accepted changes-requesting review at most once.
- Fail closed when original Developer workspace/publication-tree proof is missing or when a human controls the Agent Session.

## Implemented boundaries

1. **Atomic ACP ending fence.**
   `CloseMutationAdmissionWithPromptEvidence` records a fixed ACP stop reason or classified prompt failure in the same fenced transaction that closes mutation admission, before Runtime Process cleanup.
   This prevents a later MCP mutation from acquiring authority after the recorded ACP ending.
   The forward-only `000027_prompt_outcome_checkpoint.sql` migration retains this minimal classification across cleanup failures and orchestrator restarts without storing prompt text or credentials.
2. **Runtime stop and mutation barriers.**
   Cleanup failures and expired live leases enter the existing recovery path, which first proves the Runtime Process stopped and reconciles unsettled mutations.
   If the recorded ending is eligible and exactly one Developer or Reviewer terminal mutation succeeded, recovery reactivates the original Turn only as `CORROBORATING` and enqueues its credential-free verification checkpoint instead of scheduling another agent prompt.
   Recovery still rechecks unrelated successful effects such as comments, while the later verifier corroborates the recorded publication and terminal review evidence.
   An unrecorded or known-ineligible ending continues to the ordinary infrastructure-failure path and cannot be automatically settled from the old mutation.
3. **Durable verification.**
   `000026_terminal_intent_corroboration.sql` persists the exact original Turn, source mutation, prompt classification, safe failure category, and verifier job identity.
   The verifier acquires fresh Role-specific GitHub App credentials and rechecks PR identity/head, Reviewer review identity, and Developer normalized workspace/publication-tree equality without repeating the MCP mutation.
   The Store atomically applies a verified Turn outcome or Human Handoff under the leased verifier authority, with exactly-once successor jobs and Review Cycle accounting.
4. **Human reactivation.**
   A matching human-triggered new Attempt enqueues a no-prompt revalidation job instead of preparing an Agent Turn.
   A provenance-checked internal Workflow event accepts only freshly corroborated old evidence under the new Attempt and never rewrites the earlier failed settlement.
   A second unsuccessful human-triggered window gets a distinct, idempotently published Human Handoff; Issue closure and head synchronization fence or cancel pending revalidation.
5. **Unknown-ending confirmation.**
   A retry Turn can receive a `prior_terminal_intent` notice and explicitly call `confirm_prior_terminal_intent` for a Store-proven ancestor mutation.
   The MCP backend rechecks the PR or review, records a new internal terminal intent, and never calls GitHub `SubmitReview` for that confirmation.
   An explicitly cancelled Reviewer Turn instead receives a non-confirmable review notice directing Human Handoff; new `submit_review` calls and exact mutation replay are fenced against duplicate or ineligible native reviews.
6. **Safe diagnostics and configuration.**
   The orchestrator logs only fixed failure categories and allowlisted GitHub status values, not upstream request IDs, raw dependency errors, response bodies, or credentials.
   `OMNIGREX_TERMINAL_CORROBORATION_DURATION` defaults to 30 minutes in configuration and both Compose/environment examples; consult the [operator guide](operator-guide.md) for deployment and recovery procedures.

## Verification and release boundaries

- Focused race-enabled Developer, Reviewer, replay, cancellation, closure, restart, synchronization, reactivation, rate-limit, and migration tests pass.
- A populated v25-to-current-schema migration test preserves an already accepted review without creating a retroactive checkpoint.
- The full race-enabled Store integration suite passed with a 30-minute per-package timeout, including the v27 stopped-runtime recovery and ACP admission fence.
- `mise.toml` serializes Docker-backed integration packages to reduce concurrent host memory pressure.
- On this checkout, `mise run check` still fails on a pre-existing development/deploy Compose network-and-label mismatch and a local Git UTC timestamp formatting difference (`+00:00` versus `Z`).
- `mise run test-integration` still fails on the same unrelated Compose topology mismatch; do not run destructive `mise run test-compose` unless its stable resources may be removed.
- Re-run `mise run check`, the focused Docker-backed tests, and `mise run test-integration` after follow-up changes before release.
- The working tree also contains unrelated external `gateway` network/label changes in `compose.yaml`, an untracked `opencode.jsonc`, and an untracked runtime-profile compatibility artifact; do not include their contents or deployment secrets in this feature commit.

The live Store settlement and verifier-owned settlement intentionally have separate authority preconditions and completion fences.
Their validation, reducer transition, provenance insertion, and successor-job behavior must stay aligned; prefer parity regression tests to an unguarded shared transaction refactor.
