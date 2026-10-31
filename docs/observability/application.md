# Application Observability Contract and Deployment Handoff

## Application Contract

Use the [operator observability guide](../operators/observability.md#application-metrics) for configuration and lifecycle behavior.
The application uses OpenTelemetry and OTLP/HTTP; backend-specific configuration belongs outside this repository.
Logs remain JSON on stdout and use the existing Docker collection route.

Operation instrumentation scope is `github.com/jozala/omnigrex/operations`.
Fixed operation names are `agent_turn.prepare`, `agent_turn.execute`, `runtime_process.launch`, `agent_turn.reconcile_outcome`, `mcp.tool.execute`, `webhook.process`, `mutation.recover`, and `runtime_process.cleanup`.
Only claimed/admitted work creates these spans; idle claims, readiness checks, heartbeats, and individual Agent Events do not.

Operation-start and completion logs expose available domain IDs, Role, and Stage without fetching extra state for telemetry.
Preparation adds newly committed Participant, Session, and Turn identities at completion.
Outcome reconciliation preserves the existing `Agent Turn outcome reconciled` completion message, with domain `status` and `domain_outcome` separate from the bounded telemetry `outcome`.
Context-aware diagnostics and operation logs include `trace_id` and `span_id` only for valid contexts, including unsampled contexts.
Normal `slog.WithGroup` behavior applies to appended correlation fields; the production logger uses no group around these fields.
Do not put trace/span IDs or domain IDs into log stream labels.

Execution finalization retains execution trace identity while keeping its independent lease-cancellation semantics.
Admitted MCP mutation spans outlive HTTP disconnection and end after actual execution and durable finalization.
Preparation, execution, runtime-originated MCP requests, and later recovery can belong to separate traces; join investigations using domain IDs.
Sampling or delivery loss can leave a log's trace link without a stored trace.
Long execution spans are exported only when they end.

## Metrics

These are OpenTelemetry instrument names, before backend translation.
Only the listed attributes are exported; resource metadata is additional.

| Instrument | Kind / unit | Attributes | Semantics |
| --- | --- | --- | --- |
| `omnigrex.workflow.count` | Observable gauge / Workflows | `state` | All retained Workflow rows in each of the seven persisted domain states, including `NEEDS_HUMAN`, `PR_READY`, and `CLOSED`. |
| `omnigrex.agent_turn.active` | Observable gauge / turns | None | Turns in `STARTING`, `RUNNING`, `CANCELLING`, `SETTLING`, `RECONCILING`, or `CORROBORATING`; excludes queued and terminal turns. |
| `omnigrex.work.pending` | Observable gauge / items | `queue_category` | Due, durably unleased, domain-eligible work in the table below. |
| `omnigrex.work.oldest_pending_age` | Observable gauge / seconds | `queue_category` | Time since the oldest eligible item's availability; zero when empty. |
| `omnigrex.mutation.unresolved` | Observable gauge / mutations | None | Mutation rows in `UNKNOWN` or `RECONCILING`, including retained and closed Workflow history until actually resolved. |
| `omnigrex.mutation.oldest_unresolved_age` | Observable gauge / seconds | None | Conservative upper bound from mutation `started_at`, falling back to `admitted_at`; zero when empty. |
| `omnigrex.operation.attempts` | Counter / attempts | `operation`, `outcome` | Completed observed logical attempts, independent of trace sampling. |
| `omnigrex.operation.duration` | Histogram / seconds | `operation` | Wall-clock duration including internal retry waits and finalization. |
| `omnigrex.observation.failures` | Counter / collections | None | One increment per failed durable snapshot or collection-gate timeout. |

The unresolved-mutation age is not a precise first-UNKNOWN age: the schema does not persist that transition timestamp.
It includes execution time before ambiguity and does not reset when reconciliation changes `updated_at`.
Normal `RESERVED`/`IN_FLIGHT` work is not recovery backlog; its later transition to `UNKNOWN`/`RECONCILING` makes it eligible.
An unresolved mutation remains visible after a recovery Human Handoff; an ordinary Human Handoff without unresolved mutations is not an alert condition.

Active turns describe durable lifecycle, not live containers or occupied execution slots.
A stopped Runtime Process with a future-scheduled terminal corroboration remains active while eligible corroboration backlog is zero.
Use its domain-ID logs to distinguish this waiting lifecycle.

Counters reset on restart, may miss abrupt process death, and are not exactly-once durable completion counts.
One logical operation records one attempt and one duration; cleanup's bounded internal retries share that operation and expose `retry_count` on the span.
Later claimed retries count separately.
Nested operation durations overlap and must not be summed into total execution time.
Outcome values are `success`, `domain_outcome`, `failure`, `cancelled`, and `timeout`.
Expected blocked results are domain outcomes; infrastructure errors remain failures even if their retry or Human Handoff was acknowledged successfully.

### Queue Categories

This release observes the following explicit allowlist, not every internal job kind.
Workflow GitHub label effects, Human Handoff publication, closure/retention actions, and their escalation queues are outside these pending gauges; use their existing diagnostics when investigating those actions.

| Category | Source and eligibility | Age origin | Threshold guidance |
| --- | --- | --- | --- |
| `preparation` | Due `AVAILABLE` preparation jobs with remaining attempts, matching active Workflow revision/Stage/Role/purpose, proposal head, active Attempt, and no active turn or unsettled recovery. | `available_at` | Profile/credential reads and preparation retry configuration. |
| `execution` | Due `AVAILABLE` execution jobs with remaining attempts and active Workflow/Attempt/Participant, automation-controlled eligible Session, matching queued/starting unowned Turn and control revision. | `available_at` | Configured concurrency, turn timeout, and representative queue length; capacity does not remove pending work. |
| `runtime_cleanup` | Due `AVAILABLE` registered stop jobs for the matching inactive interrupted/reconciling Turn epoch with unsettled recovery. | `available_at` | Stop timeout, cleanup retries, and recovery polling. |
| `mutation_recovery` | Due `AVAILABLE` registered reconciliation jobs for the matching inactive interrupted/reconciling Turn epoch with unsettled recovery, a recorded runtime stop, and successful stop job. | `available_at` | External-read timeout and recovery retry configuration, accounting for time previously spent awaiting cleanup. |
| `corroboration` | Due `AVAILABLE` verification/revalidation jobs with matching checkpoint, Turn epoch, active Workflow/Attempt, and automation-controlled Session; verification requires pending corroboration, revalidation requires a handed-off failed source and matching current Stage without another active turn. | `available_at` | Terminal corroboration window and backoff. |
| `webhooks` | Due `PENDING` deliveries with remaining attempts and no earlier blocking reopen delivery/event. | `retry_at`, falling back to `received_at` | Ingress volume, processing time, and external installation-enumeration retries. |
| `historical_events` | `PENDING` normalized events with remaining attempts, no collecting retention generation, and no earlier pending event for the same Work Item. | `created_at` | Historical-event draining and Workflow processing time. |
| `label_provisioning` | Due `AVAILABLE` provisioning jobs with remaining attempts. | `available_at` | Repository onboarding volume and GitHub retry timing. |

Claimed rows are excluded even after lease expiry until existing recovery reclaims them; telemetry never reclaims leases.
Availability-age timestamps do not persist when a separate domain prerequisite became eligible, so ages can include time spent behind that prerequisite.
Counts and ages use one database snapshot and one statement timestamp, with negative ages clamped to zero.
Successful snapshots emit stable zero-valued categories; failed snapshots emit no durable gauges.
The aggregate query is tested with 20,000 retained rows in each of the Workflow, job, mutation, webhook-delivery, and normalized-event history tables without a new index; production growth remains subject to the observation deadline.

### Series Budget

Histogram finite boundaries in seconds are `0.1, 0.5, 1, 5, 15, 60, 300, 900, 3600, 7200`.
For one resource identity and classic histogram translation:

| Component | Maximum series |
| --- | ---: |
| 8 operations × 5 outcomes | 40 |
| 8 histograms × (10 finite buckets + infinity bucket + sum + count) | 104 |
| 7 Workflow states | 7 |
| 8 pending categories × count and age | 16 |
| Active turns and two unresolved-mutation gauges | 3 |
| Observation failures | 1 |
| **Application subtotal** | **171** |

The calculation includes all possible outcome combinations even though attempt series appear only after observation.
Durable categories and the observation-failure counter emit zeros when healthy and empty.
This baseline originally targeted 200 series including backend metadata; the [step-budget extension](#extended-series-budget) defines the current combined target.
Inspect actual translation, histogram format, and metadata before accepting deployment.
Resource-label changes, including build versions, can temporarily retain multiple sets of series across upgrades.
The 60-second interval reduces sample volume, not cardinality.

## Deployment Handoff

Status: application configuration and tests are local; backend configuration and live acceptance remain pending.
Use an authorized deployment agent to perform and record the following work outside this repository.

1. Preserve stdout JSON fields through the existing Docker log collection path.
   Configure log-to-trace linking using `trace_id` and trace-to-log lookup scoped to the orchestrator and exact trace ID.
   Confirm no second log-ingestion route and no high-cardinality stream labels.
2. Discover actual exported names, units, resource labels, histogram translation, and metadata.
   Create one Workflow Health dashboard with Workflow states/Human Handoffs, active turns, queue count/age, unresolved mutation count/age, operation outcome rates/increases, duration distributions, and domain-ID log/trace links.
   Keep infrastructure panels in the existing infrastructure dashboards.
3. Configure three alerts with deployment-specific thresholds and sustained observation windows.
   Alert on eligible pending age beyond the category's waiting threshold, unresolved mutations beyond the recovery window adjusted for the conservative age definition, and repeated launch/cleanup/reconciliation failures.
   Unrelated operation successes must not suppress queue alerts.
   Include `failure` and relevant `timeout` outcomes, not ordinary `domain_outcome` or one expected retry.
4. Treat missing durable gauges as unavailable observations, never zero backlog or successful recovery.
   A positive increase in `omnigrex.observation.failures` distinguishes exported database-observation failures from total application/export/collector outage.
   Use continuously emitted expected gauges and existing infrastructure health signals to detect total absence after an interval allowing collection and ingestion delay.
   Configure explicit unknown/no-data handling for each application alert; never silently resolve an existing alert because collection stopped.
   Set notification destinations only with deployment authorization.
5. Run one operator-approved real Agent Turn plus a failure/retry example.
   Confirm build/resource identity, parent relationships, detached mutation lifetime, domain-ID searches across independent traces, bidirectional navigation, dashboard values, and alert behavior.
   Verify idle and busy ingestion, actual added series, and upgrade churn, allowing for reporting delay rather than using month-to-date cost as an immediate signal.

Record dashboard and alert identifiers, thresholds and their rationale, observed series/ingestion, tested workloads, and any remaining live verification in the deployment's own records.

## Agent Turn step-budget metrics

The controller consumes runtime-neutral Agent Events from the existing live Pi RPC path through the same optional OpenTelemetry provider.
Pi contains no exporter or telemetry credentials.
The owned budget module remains the only iteration counter; failed/cancelled requests and the reserved summary iteration retain the [step-budget accounting semantics](../reference/agent-step-budgets.md).

| Instrument | Kind / unit | Dimensions | Meaning |
| --- | --- | --- | --- |
| `omnigrex.agent_turn.step_limit` | Histogram / `{iteration}` | `role` | Effective finite ceiling, once per observed prompt ending; never a label. |
| `omnigrex.agent_turn.model_iterations` | Histogram / `{iteration}` | `role`, `budget_mode`, `completeness` | Admitted iterations including summary; partial counts are lower bounds. |
| `omnigrex.agent_turn.budget_utilization` | Histogram / `1` | Same | Per-Turn consumed/limit ratio, finite budgets only. |
| `omnigrex.agent_turn.budget_observations` | Counter / `{turn}` | Same plus `termination` | Usage coverage and runtime ending, including unknown usage. |
| `omnigrex.agent_turn.budget_summary` | Counter / `{turn}` | `role` | Summary-only entry, independent of completion verdict. |
| `omnigrex.agent_turn.budget_handoffs` | Counter / `{turn}` | `role` | Successfully settled incomplete-work step-budget Human Handoffs. |

Role is `DEVELOPER`, `REVIEWER`, or `unknown`, never a Profile name.
Budget mode is `finite`, `unlimited`, or `unknown`; completeness is `complete`, `partial`, or `unknown`.
Unknown usage produces a coverage counter but no synthetic zero usage or ratio; known finite configuration can still produce a ceiling value.
Unlimited budgets expose actual usage without a fabricated ceiling or utilization.
Termination is `completed`, `failed`, `cancelled` (including caller deadline), `transport`, or `unknown`; this is the runtime prompt ending, not the Work Item verdict.
A successful summary can still settle successfully through terminal MCP corroboration.
Missing final observation leaves a partial lower bound even when the last known value is zero.
A controller crash can prevent even the coverage counter: compare with durable active Turns and diagnostics, not an assumed complete usage ledger.

Usage/limit histogram boundaries are `1, 2, 5, 10, 20, 50, 100, 200, 500, 1000` iterations.
Utilization boundaries are `0.1, 0.25, 0.5, 0.75, 0.9, 1`.
The utilization histogram aggregates equally weighted per-Turn ratios; sum/count is mean per-Turn utilization, not total consumed divided by total allowance.
Filter `completeness=complete` for exact usage distributions and inspect partial distributions separately.
High complete utilization and rising summary entry indicate pressure even without handoffs; ordinary low-utilization early completion does not imply a constraining ceiling.
Compare handoff rate and correlated settlement logs before increasing ceilings; rising `transport`/`failed` endings call for a different investigation.
Use correlated logs for individual configuration values and Turn/Session/Participant identities.
No raw provider/tool content, arbitrary metadata, credentials, summaries, Profile names, repository names, or domain IDs are metric attributes.

### Delivery and retention

Delivery is best-effort live observation, not durable or exactly-once accounting.
The adapter validates the existing six-field schema (1024-byte data bound, closed fields, safe integer ranges and monotonic lifecycle); the full control-frame bound remains 1 MiB.
Critical completion/exhaustion evidence remains independent in the adapter's outcome snapshot and existing settlement evidence.
A separate one-slot coalescing observation queue cannot displace critical events; consumers may lose intermediate observations without changing enforcement.
The controller uses the final critical snapshot to recover missed summary pressure and usage.
Each per-prompt consumer retains only two flags, deduplicates summary and final usage within that lifetime, and records SDK aggregates without network export waits.
Disabled or failing export does not change settlement or enforcement.
Ordinary downstream event-sink failures retain their existing behavior; the metric consumer introduces no exporter into that critical path.
History Replay emits separate `history_*` events, never live budget observations; it neither reconstructs the counter nor duplicates metrics.
No observations are appended to Pi Session history, avoiding per-iteration persistence and additional retained volume.
Future observation kinds can use the same transport-to-neutral-event boundary and controller consumers without another service.
Abrupt controller death, exporter failure, and uncertain settlement acknowledgement can lose samples, including handoff counts.

### Extended series budget

The earlier 171-series subtotal remains the non-budget baseline.
This extension adds at most 360 classic series per resource: 105 coverage categories (3 Roles × 7 mode/completeness combinations × 5 terminations), 156 usage series (12 combinations × 13), 39 finite-ceiling series (3 × 13), 54 ratio series (6 × 9), and 6 summary/handoff counters.
The seven coverage combinations are finite/unlimited with complete/partial/unknown usage, plus unknown mode and unknown usage.
The application subtotal is now **531**, replacing the earlier 200-series target with a **600-series target**, allowing 69 backend metadata series.
Termination is deliberately excluded from histogram dimensions; compare ending-category counts and correlated logs rather than individually joining aggregate histogram samples.
Resource identity churn still multiplies retained backend series.
