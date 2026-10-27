# Agent Turn step budgets

[Repository configuration](repository-configuration.md) · [Deployment configuration](deployment-configuration.md)

## Configure a ceiling

A step is one admitted conversational model request and its response, potentially followed by multiple tools.
It is not a tool-call, token, shell-command, elapsed-time, or cost limit.
Use a ceiling as runaway-work protection, not a target.

```yaml
# .omnigrex/team/developer.md front matter
max_steps: 100
```

An explicit `max_steps: unlimited` overrides a finite operator fallback.
Omitting the field inherits `OMNIGREX_AGENT_MAX_STEPS`; an unset environment variable means unlimited.
The operator fallback is not a security cap on trusted repository configuration.
Both boundaries reject zero, negative, fractional, unknown, overflowing, and empty values.
Profile integers are YAML integers, not quoted numeric strings; `unlimited` is the only accepted string.
The finite implementation bound is 2147483647, representable on 32-bit Go and exactly in JavaScript.

Preparation selects the named Profile from one pinned latest default-branch snapshot using the existing selection lifecycle.
The Profile's canonical snapshot preserves omitted `max_steps` versus explicit unlimited.
The resolved budget is recorded separately in the prepared Turn's `turn_configuration.step_budget`, using the controller fallback in effect at preparation.
A running or queued prepared Turn never rereads checkout configuration or recomputes against a changed fallback.
After changing an operator fallback, restart the controller through the normal deployment procedure; subsequently prepared inherited Turns use it.
Session Continuation receives a fresh counter at the next admitted prompt, regardless of prior history.
History Replay is metadata presentation and never reconstructs a live budget.

## Runtime semantics

A finite limit N permits at most N conversational iterations total.
Iterations before N retain configured tools; already admitted tools finish before iteration N.
Iteration N removes all provider-visible tool declarations and executable local/MCP access, including nested deferred-tool access.
Only this reserved iteration receives the summary instruction: completed work, unfinished work, blockers, permission to be incomplete, and a prohibition on claiming unfinished work is complete.
Ordinary requests receive no numeric allowance, countdown, or urgency instruction.
A limit of one permits only a tool-free summary.
Early conversational completion does not force a reserved budget-summary request.

The owned runner chains Pi's supported SDK hooks; it does not fork the loop or introduce a runtime service.
At the final response boundary it ends the inner loop and synchronously requests public Session abort without awaiting idle inside the callback.
This also stops queued-message and pre-settlement outer continuations.
Pi 1.4.0 native compaction maintenance is not a conversational iteration and never resets or replenishes the allowance.
Pre-prompt compaction, between-request threshold compaction, and overflow compact-and-retry share the admitted prompt's counter; the retried conversational request consumes the next iteration.
See [native compaction](../operators/context-compaction.md) for its separate provider cost, cancellation, and failure behavior.
Queued continuation input left by this stop is cleared rather than executed against another Turn's allowance.
Configured tools are restored after activity settles; summary transcript projections are request-only and never become stale Session instructions.

## Completion and recovery

Summary-only entry does not prove failure.
Existing durable terminal MCP evidence, mutation settlement, publication/workspace captures, fencing, and fresh corroboration still determine success.
The summary's wording cannot establish completion.
When positive, complete budget settlement leaves required work incomplete, the existing blocked outcome immediately stops automation in Human Handoff, without an infrastructure retry and fresh allowance.
Diagnostics include the effective limit, usage, reason, and bounded/redacted agent summary when available.
Inspect the published/committed work before deciding whether to reactivate; a new Turn has a fresh allowance, not proof that prior work was complete.
See [Human Handoffs](../users/human-handoffs.md).

Provider errors, explicit cancellation, transport loss, and token-length limits are not positive step exhaustion.
They retain existing failure/recovery policies even when summary-only mode was entered.
Unavailable GitHub, database, or workspace corroboration and not-yet-visible reviews remain uncertain completion under the existing corroboration-recovery policy, not positive evidence of incomplete-work exhaustion.
A request admitted at the SDK request-preparation boundary consumes an iteration even if routing/provider transport fails or cancellation prevents a successful response.
Finalized error/cancelled responses increment the response count; missing responses never count as successful work.

## Typed observation contract

`internal/stepbudget.Limit` is the validated finite-or-unlimited type.
`agent.OutcomeEvidence` is the durable settlement schema, including the controller-only `StepBudgetExhausted` fact.
`internal/stepbudget.Observation` is the enforcement counter's bounded source of truth, translated by the Pi adapter into `agent.Outcome.StepBudget` and `agentevent.OperationalMetadata.StepBudget`.
The neutral event contract exposes the same schema as `agentevent.StepBudgetObservation`, reused by terminal outcomes without coupling the agent interface directly to configuration packages.
Consumers, including #112, must reuse this evidence rather than parse log text or maintain another counter.
The owned runner captures its framed writer before Pi installs the extension stdout guard, keeping budget evidence on the controller transport rather than stderr.
Queue cleanup and tool restoration run before the externally visible settlement receipt, so an immediate next prompt cannot race late queue events.
The owned JSONL record `omnigrex_step_budget` carries a `data` object:

| Field | Meaning |
| --- | --- |
| `limit` | Positive integer or `"unlimited"` resolved at preparation |
| `used` | Admitted conversational requests, including a reserved summary request |
| `responses` | Finalized responses, including error/cancellation; never greater than `used` |
| `summary` | Entry into the reserved summary-only request, independent of completion |
| `stopped` | Successful final response stopped by the ceiling, not yet an incomplete-work verdict |
| `complete` | Live usage observation reached Session settlement, not Work Item completion |

Usage counters are bounded by JavaScript's largest safe integer (9007199254740991); representational overflow is a protocol failure, never positive exhaustion.
The adapter validates field presence, types, bounds, monotonicity, and agreement with the prepared budget.
A configured runtime missing settlement evidence is a protocol failure, not invented zero usage or exhaustion.
The optional `summary` record field accepts at most 2048 UTF-8 bytes of untrusted text, used only by the credential-redacting settlement diagnostic path, never as a completion claim or metric label.
Oversized summaries are omitted wholesale; runtime truncation or normalization before controller credential redaction could leak a credential fragment.
The controller redacts full accepted text before control-character normalization and final diagnostic truncation.
No metric export or provider/tool payload is required for enforcement or settlement.
For an admitted terminal response the existing Turn outcome retains the minimal budget observation alongside admission/status evidence.
Failed or uncertain transport outcomes report the last observation through correlated usage diagnostics, without inventing complete durable prompt evidence.
Only an incomplete-work budget handoff records `step_budget_exhausted: true`; successful terminal corroboration preserves success without that flag.
No new usage-history database, exporter, backend credentials, or aggregate instruments are introduced.

## Per-Turn diagnostics

Correlate the `Agent Turn step budget` observations with `agent_turn_id`, `agent_session_id`, Participant, execution epoch, and control revision.
They report finite/unlimited configuration, requests, responses, summary pressure, budget stop, and observation completeness.
A crash or transport failure can leave only a partial lower bound; `complete=false` must not be interpreted as an exact total, even when the observed lower bound is zero.
No observation means usage is unknown, not zero.
Use the correlated settlement domain outcome to distinguish successful completion from `step_budget_exhausted` Human Handoff.
Do not classify every summary entry as a failed Turn or an exhausted-work handoff.
Aggregate operator metrics are a separate delivery in [Work Item 112](https://github.com/jozala/omnigrex/issues/112).

## Runtime compatibility and qualification

New qualified Runtime Profiles inventory the owned `step-budget.mjs` alongside the pinned runner and packages.
Existing Participants retain their immutable image/resource contracts.
Finite budgets against a historical inventory without budget support fail preparation as a prerequisite, rather than silently rebinding an image or running unbounded.
Newly prepared historical unlimited Turns still record their effective unlimited configuration, but their runtime cannot supply usage observations; absence remains explicitly unknown.
Already prepared historical Turns without a resolved snapshot are not retroactively assigned the new operator fallback.
Select a newly qualified Participant binding through the existing reassignment lifecycle to enable the feature for such history.

The shared protocol configuration gate executes durable controlled-provider SDK regression tests and production bootstrap tests on Docker and Microsandbox.
The existing `TestPiControlledWorkflow` CI selection also includes bounded-work Human Handoff and successful terminal-evidence summary-boundary subtests.
Run local Docker gates; executable Microsandbox qualification belongs to the PR's Linux/KVM CI job, not nested virtualization inside an Agent Turn.
Passing local Docker tests alone does not prove native startup, packaging, transport, cancellation, or lifecycle compatibility.
