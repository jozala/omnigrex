# Application Observability: First Release

## Goal

Deliver a small but complete visibility loop for Omnigrex: correlated JSON logs, meaningful operation traces, a bounded application-metrics set, and one deployment-side dashboard with actionable alerts.
An operator should be able to determine what is running, what is waiting, what failed, and where to investigate.
Implement all sections as part of this release, rather than treating application metrics as an optional follow-up.

The application and all repository documentation must remain vendor agnostic.
Use standard OpenTelemetry APIs and OTLP/HTTP for traces and application metrics.
Continue writing logs to stdout for collection through Docker; do not add direct OpenTelemetry log export or a second log-ingestion route.

## Read First and Confirm the Starting Point

Read `AGENTS.md`, `README.md`, `CONTEXT.md`, the relevant ADRs, and `docs/operator-guide.md`.
Use the domain terminology in `CONTEXT.md` and preserve the existing ownership, fencing, retry, and shutdown contracts.
Start implementation from current `main`, checking whether the existing telemetry and readiness-suppression changes have already merged.
Do not change `docs/plans/first-iteration.md`.

At the time of planning, Omnigrex already has:

- Optional batched OTLP/HTTP trace export in `internal/telemetry/telemetry.go`.
- Incoming HTTP instrumentation for webhook and MCP requests and outgoing GitHub client spans.
- Docker client instrumentation, with no-op tracer providers for readiness and exact-image diagnostic clients.
- JSON stdout logging through `slog`.
- Docker-log collection and infrastructure/database monitoring in the deployment.

Live traces were inspected and confirmed to contain service name, namespace, and environment attributes.
Two real MCP traces contained GitHub client spans whose parent IDs matched the MCP server span.
The observed service version was `dev`, so published/deployed builds need a meaningful version value.
Do not assume those observations prove correlation for every mutation, detached operation, or later recovery attempt.

Infrastructure metric collection was changed to 60 seconds, with a goal of leaving headroom within a deployment allowance of approximately 10,000 metric series.
These are deployment observations, not application limits or values to hard-code into Omnigrex.

## Scope and Delivery

Deliver two focused application PRs, followed by deployment-side configuration and verification:

1. Context-correlated logging, operation spans, and meaningful build identification.
2. Application metrics, bounded durable-state observations, and metrics lifecycle/configuration.
3. Log/trace navigation, one Workflow Health dashboard, and three application alerts.

Do not add a new observability service, a new public metrics endpoint, tracing database columns, or durable trace-context propagation in this increment.
Do not add broad database tracing, model-call tracing, prompt capture, per-token events, or instrumentation for every internal function.
Do not expand infrastructure metric collection or change the deployment subscription.

## 1. Trace-Correlated Structured Logs

Add a context-aware `slog.Handler` wrapper in `internal/telemetry/` around the existing JSON handler.
Enrich records with `trace_id` and `span_id` only when the supplied context has a valid span context.
Preserve normal handler behavior, including `Enabled`, `WithAttrs`, `WithGroup`, log levels, and existing record attributes.
Do not require tracing to be enabled for normal logging to work.

Use context-aware logging at the instrumented operation boundaries and for existing diagnostics emitted inside them.
Adding the handler alone is insufficient because calls such as `logger.Info` do not pass an operation context.
Provide concise start/finish messages where existing logs do not already express the operation lifecycle.
Use existing completion messages rather than adding duplicate logs for the same event.

Include applicable `workflow_id`, `agent_participant_id`, `agent_session_id`, `agent_turn_id`, `role`, and `stage` fields when available.
Preparation can fail before a Participant or Agent Turn exists; do not fabricate IDs or query additional state solely to fill optional fields.
These values belong in structured log fields and span attributes, not metric labels or log stream labels.

Inspect these entry points:

- `cmd/omnigrex/main.go`: JSON logger construction, `loggingAgentEventSink`, and `loggingOutcomeReconciler`.
- `internal/agentturn/outcome_reconciler.go`: existing safe GitHub observation diagnostics.
- The operation boundaries identified below.

Do not undertake a repository-wide logging rewrite.
Update error callbacks where necessary for the scoped operations, while preserving existing sanitized error reporting and avoiding duplicate failure messages.

## 2. Operation Spans

Use one instrumentation scope and fixed span names.
Create spans only for acquired or admitted work, not empty queue polls, readiness checks, lease renewal, or individual Agent Events.

| Span name | Boundary and likely implementation entry point |
| --- | --- |
| `agent_turn.prepare` | One claimed preparation job in `internal/agentturn/worker.go`; include repository credentials/profile loading and durable acknowledgement. |
| `agent_turn.execute` | One acquired execution attempt in `internal/agentturn/execution_worker.go`, including finalization. |
| `runtime_process.launch` | The launch call in execution, including workspace/runtime setup and ACP initialization through `internal/agentturn/runtime.go`. |
| `agent_turn.reconcile_outcome` | One reconciliation attempt in `internal/agentturn/outcome_reconciler.go`, including ledger reads and GitHub evidence retrieval. |
| `mcp.tool.execute` | One validated tool invocation in `internal/mcp/gateway.go`, including the admitted mutation's actual execution lifetime. |
| `webhook.process` | One claimed delivery or historical event in `internal/github/webhook/processor.go`. |
| `mutation.recover` | One claimed mutation-recovery operation in `internal/mcp/recovery_worker.go`. |
| `runtime_process.cleanup` | One logical execution cleanup operation, including bounded retries; inspect execution cleanup and the stale-runtime stop worker. |

Avoid duplicate reconciliation spans where `Reconcile` delegates to `ReconcileRecorded`.
Cover direct recorded-evidence reconciliation through a shared internal boundary or equivalent narrowly scoped structure.
Do not add wrappers around every cleanup retry; use a logical cleanup span with a safe retry count where appropriate.

Pass child contexts into existing GitHub and Docker calls so their client spans nest under the operation making them.
Execution launch and outcome reconciliation should normally be children of execution.
Preserve span context through independently cancelled work/finalization contexts without changing their cancellation semantics.
MCP mutations must continue independently of HTTP disconnection after admission; their operation spans must end when admitted work finishes, not when the HTTP handler stops waiting.
Do not change retries, lease validation, authority admission/draining, mutation finalization, or Workflow transitions to accommodate telemetry.

Preparation, execution, MCP requests, and later recovery may remain separate traces connected by domain IDs.
Do not promise that Runtime Process MCP requests become children of execution without propagation across that runtime boundary.
Long execution spans are exported on completion; start logs and shorter child spans provide visibility while work remains active.

Attach the available domain IDs, Role, Stage, bounded operation outcome, and safe failure category.
For MCP, include the validated tool name as a span attribute, never arguments or response bodies.
HTTP `200` does not imply tool success: inspect the actual result path and mark real tool failures appropriately.
Expected domain outcomes such as a blocked result or Human Handoff are not automatically telemetry errors.
Handle cancellation, timeout, and infrastructure failure explicitly, reusing existing classification/redaction.
Never record unsanitized `err.Error()` or call `RecordError` on an error that may contain credentials, prompts, or response content.

## 3. Application Metrics

Add an OTLP/HTTP metrics exporter and SDK meter provider alongside the existing trace provider.
Share service/resource identity, register the provider early, and flush/shut it down after producers stop with a fresh bounded context.
Exporter failures must not stop Workflow processing.
Keep tracing-only, metrics-only, disabled, and endpoint-unconfigured behavior explicit and testable.
Use standard signal-specific endpoint/header configuration and document opt-in/export behavior without changing the existing tracing contract accidentally.
Pass supported configuration through both Compose bundles and update both environment examples.
Default metrics export to 60 seconds and allow the standard export-interval override.

Export only the defined application instruments initially.
Inspect existing HTTP and other library instrumentation before installing the global meter provider; use SDK views to drop unplanned instrumentation scopes or instruments.
Do not inadvertently activate a large additional library metric set.

### Instruments and Semantics

The names below are OpenTelemetry instrument names; verify the backend's metric-name translation when configuring dashboards.

| Instrument | Kind | Attributes | Meaning |
| --- | --- | --- | --- |
| `omnigrex.workflow.count` | Observable gauge | `state` | Durable current Workflow count by domain state, including Human Handoff states. |
| `omnigrex.agent_turn.active` | Observable gauge | None | Durable nonterminal executing turns; explicitly define inclusion of starting, running, settling, and reconciling states. |
| `omnigrex.work.pending` | Observable gauge | `queue_category` | Eligible, unleased work in a fixed allowlist of queue categories. |
| `omnigrex.work.oldest_pending_age` | Observable gauge, seconds | `queue_category` | Age since eligible availability for the oldest pending item, not age of future-scheduled work. |
| `omnigrex.mutation.unresolved` | Observable gauge | None | Durable mutations awaiting reconciliation; explicitly define qualifying states and closure/retention scope. |
| `omnigrex.mutation.oldest_unresolved_age` | Observable gauge, seconds | None | Age since the qualifying unresolved transition, using persisted evidence where available. |
| `omnigrex.operation.attempts` | Counter | `operation`, `outcome` | Completed logical operation attempts; later claimed retries count separately. |
| `omnigrex.operation.duration` | Histogram, seconds | `operation` | Duration of the selected logical operation attempts, without outcome or domain-ID dimensions. |
| `omnigrex.observation.failures` | Counter | None | Failed durable-state observations, so a broken collector does not look like healthy empty queues. |

Use the fixed operations listed in the span table, not dynamic tool names as metric attributes.
Use a small outcome vocabulary, such as success, domain outcome, failure, cancelled, and timeout, with explicit mapping to existing domain results.
Expected Human Handoffs must not be conflated with infrastructure failure.
Operation counters describe observed attempts, not exactly-once lifetime counts of durable Agent Turn completions.
They reset on process restart and may miss an abrupt crash; document this rather than adding durable telemetry ledgers.
Record one counter increment and one duration observation per completed operation span, independently of trace sampling.
Internal retries remain inside that operation and contribute a bounded retry-count span attribute.
A later claimed retry is a separate operation; nested operation durations are not additive.

Select a small explicit histogram bucket set that covers subsecond calls, seconds/minutes, and longer execution attempts.
Calculate the resulting series count, including buckets, sum/count series, zero-valued combinations, and exported metadata.
Target approximately 100–200 additional series for one orchestrator; trim dimensions or instrument coverage before exceeding that target.
Do not attach Role, Stage, and outcome to every instrument.

### Durable-State Observations

Add a narrow aggregate observation API in `internal/store/`, backed by existing tables and domain definitions.
Inspect `jobs.go`, `turns.go`, Workflow state queries, and migrations before defining qualifying rows.
In particular, jobs use `AVAILABLE`/`LEASED` states and `AvailableAt`; future availability is not actionable backlog.
Exclude cancelled/obsolete work using the same domain eligibility rules as processing, rather than only filtering a status column.
If no existing timestamp accurately captures unresolved age, document the chosen approximation and its meaning instead of silently substituting an unrelated timestamp.

Define a fixed queue-category table before implementation.
For each category, record the source rows, domain eligibility predicate, timestamp used for eligible age, and deployment guidance for selecting a waiting threshold.
Explicitly address terminal corroboration verification/revalidation jobs.
Work waiting for execution capacity remains pending and contributes to its age.
The active Agent Turn gauge describes durable lifecycle state and must not be interpreted as occupied execution capacity.
This release does not attempt to infer queue progress from operation completion counters.

Use bounded, read-only aggregate queries once per collection, not one query per label value or per Workflow.
Prefer a consistent observation time/snapshot for related counts and ages.
Return zero for genuinely empty counts/ages, emit stable categories, and clamp negative ages caused by clock differences.
On database failure, omit affected observations and increment the observation-failure counter; do not emit false zeros or silently reuse stale values as current.
Do not acquire worker leases or hold database transactions across metric export.
No migration should be needed unless query analysis demonstrates a necessary index; any migration must be forward-only.

### Implementation Contracts

Metrics require explicit opt-in through `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` or `OTEL_METRICS_EXPORTER=otlp` with a generic endpoint.
`OTEL_METRICS_EXPORTER=none` disables metrics; an existing generic trace endpoint alone does not enable metrics.
Preserve the current trace enablement contract and disable both signals with `OTEL_SDK_DISABLED=true`.
Malformed initialization configuration fails startup with a safe diagnostic; delivery failures after initialization do not stop processing.

Observe all durable metric families in one read-only snapshot with a short deadline covering connection acquisition and queries, with no overlapping collections.
On snapshot failure, omit all durable gauges and increment `omnigrex.observation.failures` once per failed collection.
Review aggregate query plans against retained history; do not hold the snapshot transaction during export.
Missing expected gauge samples indicate unavailable telemetry, distinct from a successfully exported observation-failure increment.

Deferred terminal corroboration attempts include context loading, credentials, evidence reads, and durable acknowledgement within one `agent_turn.reconcile_outcome` span.
Direct reconciliation uses the same logical boundary without nested duplicate spans.
Active turns include `CORROBORATING` and `CANCELLING`, in addition to starting, running, settling, and reconciling; they do not represent live runtime capacity.
Scheduled verification is not eligible backlog until due; the active-turn gauge and domain-ID log lookup expose that waiting lifecycle.

Use `success`, `domain_outcome`, `failure`, `cancelled`, and `timeout` as the operation outcome vocabulary.
Expected blocked results and ordinary Human Handoffs are domain outcomes; infrastructure failures retain failure classification even when durably acknowledged or handed off.
Lost authority is cancellation with a safe fence-loss category; unknown mutation outcomes are failures unless cancellation or deadline expiry caused them.
Cached MCP replays describe the result of that invocation, not a new external mutation.

## 4. Build Identity

`Dockerfile` already accepts `ARG VERSION=dev` and injects it into `main.version`.
Inspect release CI and local/deployment builds to ensure published images pass a meaningful release version or commit SHA.
Preserve `dev` for genuinely unversioned local builds.
Reuse this identity for startup logs, trace resources, and metrics resources.

## 5. Deployment-Side Navigation, Dashboard, and Alerts

Keep backend-specific configuration outside the application repository.
The implementation agent should provide generic operator documentation and a handoff describing required fields/instruments; a deployment-authorized agent applies the concrete backend changes.
If live access is unavailable, report this part as pending rather than claiming the release is fully verified.

Preserve JSON fields through Docker-log collection.
Configure log-to-trace extraction/linking for `trace_id` and trace-to-log lookup scoped to the orchestrator and matching trace ID.
Keep domain IDs and trace/span IDs out of stream labels and do not enable duplicate log ingestion.

Create one Workflow Health dashboard using the actual exported metric names:

- Workflows by state and Human Handoffs.
- Active Agent Turns.
- Pending count and oldest eligible age.
- Unresolved mutations and oldest unresolved age.
- Recent operation outcomes and durations, using restart-aware counter rates/increases.
- Links to relevant logs and traces.

Reuse existing infrastructure dashboards for CPU/memory, container health, and PostgreSQL.
Do not add another broad infrastructure dashboard.

Start with three application alerts:

1. **Eligible pending work exceeds its queue-specific waiting threshold.**
   Alert when a queue category has eligible pending work whose oldest eligible age exceeds a deployment-selected threshold for a sustained observation window.
   Select thresholds using that category's processing behavior, configured concurrency, relevant timeouts, and representative workload.
   Work waiting for execution capacity remains pending and contributes to its age.
   Successful operations elsewhere must not suppress the alert.
   Exclude future-scheduled and obsolete work using the documented eligibility rules.
   Missing or failed observations must not be interpreted as an empty queue or successful recovery.
2. Unresolved mutations remain beyond the expected recovery window.
3. Repeated launch, cleanup, or reconciliation failures within an observation window.

Select thresholds from deployment timeouts and representative behavior, not arbitrary hard-coded universal durations.
Do not fire on one expected retry or an ordinary Human Handoff.
Define missing-data/observation-failure handling so an unavailable collector is not interpreted as a healthy empty system.
Set notification destinations only with deployment authorization.

## Verification and Acceptance

Add meaningful tests for the new seams, reusing existing worker, gateway, redaction, and store fixtures.

- JSON logs contain matching trace/span IDs only with valid context, retain fields/groups/levels, and never expose supplied secret fixtures.
- Execution, launch, and reconciliation have correct parent relationships and existing client calls receive the corresponding context.
- Empty worker polls and readiness probes create no new operation telemetry.
- Admitted MCP mutations retain context and complete their spans after HTTP cancellation without changing drain/fencing semantics.
- Tool failures returned as HTTP `200` produce failure outcomes while expected domain results remain distinct.
- Every span ends on success, failure, cancellation, timeout, and retry paths.
- Metrics observation queries correctly handle empty state, scheduled work, claimed work, obsolete work, unresolved recovery, database failure, and restart.
- Histogram/attribute configuration respects the planned series budget and unplanned library metrics are excluded.
- Provider initialization, endpoint/header settings, periodic export, and bounded shutdown work for supported enablement combinations.

Run the narrowest relevant race-enabled package tests, `mise run check`, and `mise run test-integration` when store or Docker-backed behavior is affected.
Run `mise run test-compose` only when its destructive network/volume recreation is authorized.

After deployment, verify one real Agent Turn and a failure/retry example, using an operator-approved workload.
Confirm resource metadata and meaningful version, operation nesting, domain-ID search across independent traces, log/trace navigation, dashboard values, and alert behavior.
Inspect current ingestion and added series during a representative busy period as well as an idle period.
Account for delayed usage reporting; do not use historical month-to-date cost as the immediate pass/fail signal.

The release is complete when the operator can follow an Agent Turn from preparation through execution and outcome, see its relevant logs, identify backlog/recovery problems, and do so within the agreed telemetry-volume budget.
Report implemented scope, test results, series-budget calculation, deployment configuration performed, and any live verification still pending.
