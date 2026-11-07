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
| `omnigrex.workflow.count` | Observable gauge / Workflows | `state` | All retained Workflow rows in each of the nine persisted domain states, including `PLANNING`, `PLAN_READY`, `NEEDS_HUMAN`, `PR_READY`, and `CLOSED`. |
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
| 9 Workflow states | 9 |
| 8 pending categories × count and age | 16 |
| Active turns and two unresolved-mutation gauges | 3 |
| Observation failures | 1 |
| **Application subtotal** | **173** |

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

The orchestrator consumes runtime-neutral Agent Events from the existing live Pi RPC path through the same optional OpenTelemetry provider.
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

Role is `DEVELOPER`, `REVIEWER`, `PLANNER`, or `unknown`, never a Profile name.
Budget mode is `finite`, `unlimited`, or `unknown`; completeness is `complete`, `partial`, or `unknown`.
Unknown usage produces a coverage counter but no synthetic zero usage or ratio; known finite configuration can still produce a ceiling value.
Unlimited budgets expose actual usage without a fabricated ceiling or utilization.
Termination is `completed`, `failed`, `cancelled` (including caller deadline), `transport`, or `unknown`; this is the runtime prompt ending, not the Work Item verdict.
A successful summary can still settle successfully through terminal MCP corroboration.
Missing final observation leaves a partial lower bound even when the last known value is zero.
An orchestrator crash can prevent even the coverage counter: compare with durable active Turns and diagnostics, not an assumed complete usage ledger.

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
The orchestrator uses the final critical snapshot to recover missed summary pressure and usage.
Each per-prompt consumer retains only two flags, deduplicates summary and final usage within that lifetime, and records SDK aggregates without network export waits.
Disabled or failing export does not change settlement or enforcement.
Ordinary downstream event-sink failures retain their existing behavior; the metric consumer introduces no exporter into that critical path.
History Replay emits separate `history_*` events, never live budget observations; it neither reconstructs the counter nor duplicates metrics.
No observations are appended to Pi Session history, avoiding per-iteration persistence and additional retained volume.
Future observation kinds can use the same transport-to-neutral-event boundary and orchestrator consumers without another service.
Abrupt orchestrator death, exporter failure, and uncertain settlement acknowledgement can lose samples, including handoff counts.

### Extended series budget

The 173-series subtotal remains the non-budget baseline.
This extension adds at most 480 classic series per resource: 140 coverage categories (4 Role categories × 7 mode/completeness combinations × 5 terminations), 208 usage series (16 combinations × 13), 52 finite-ceiling series (4 × 13), 72 ratio series (8 × 9), and 8 summary/handoff counters.
The seven coverage combinations are finite/unlimited with complete/partial/unknown usage, plus unknown mode and unknown usage.
The step-budget subtotal is **653**, which replaced the earlier 200-series target with a 750-series target and 97 backend metadata series of allowance.
The [prompt/tool/storage extension](#revised-bounded-series-budget) defines the current combined target.
Termination is deliberately excluded from histogram dimensions; compare ending-category counts and correlated logs rather than individually joining aggregate histogram samples.
Resource identity churn still multiplies retained backend series.

## Prompt, tool, and storage observations

These extend the existing exporter; recording is synchronous SDK aggregation, independent of trace sampling and without network waits in prompt processing, lease handling, settlement, or cleanup.
No Pi exporter, credentials, public scrape endpoint, OTel log route, or new service identity is added.
Like budget observations, these are best-effort samples, not durable accounting; process death and failed export lose observations without recovery or replay-derived metrics.

### Prompt boundary and provenance

`omnigrex.agent_turn.prompt_duration` is a histogram in seconds with the operation duration buckets.
It surrounds only the existing `Prompt` call: submission through the returned terminal outcome includes model work, tools, compaction, and internal waits, but excludes launch and subsequent finalization/cleanup.
Its only dimension is `boundary`: `runtime_terminal` when a terminal outcome is returned without a transport/caller error, otherwise `caller_return`.
A cancellation/error return does **not** establish when guest execution stopped.
This does not change durable `started_at` or `agent_turn.execute`, which still measures the acquired execution attempt including launch, cleanup, and finalization.
These nested/overlapping durations are not additive.

Execution traces and correlated completion logs include the Turn's original Profile source commit and retained source SHA-256, exact Participant Runtime Profile name/version/content hash/image, Role, Stage, and purpose.
The original source hash is **not** recalculated from decoded snapshot JSON.
Pi preparation adds `configured_model`, `configured_thinking`, and `composed_guidance_sha256`; successful existing model validation adds `effective_model` and allowlisted `effective_thinking` without an extra runtime request.
Prompt processing also records the retained configured step-budget value when available, not as a metric label.
Runtime launch adds backend family, configured memory bytes and PID limit; these are configuration, not observed usage or proof of enforced effective resources.
The composed fingerprint covers exactly the bytes produced by `agentinstructions.Operator.Compose`: platform/Stage/purpose/operator/Profile guidance.
It excludes the full Pi-rendered system prompt, repository AGENTS.md files (read explicitly through tools), skill bodies, and later instruction-bearing reads.
There is no instruction-read manifest, payload inspection, historical instruction authority, or hash of credential-bearing launch configuration.
These allowlisted fields and identities belong only in correlated traces/logs, never metric or Loki stream labels.
The existing typed failure diagnostics from decision 0046 remain unchanged.

### Shared tool producer contract

| Instrument | Kind / unit | Dimensions |
| --- | --- | --- |
| `omnigrex.tool.calls` | Counter / `{call}` | `layer`, `tool`, `outcome` |
| `omnigrex.tool.duration` | Histogram / seconds | `layer`, `tool` |

`telemetry.RecordTool(ctx, layer, name, outcome, duration)` records **one observed ending**, not a start/end pair or exactly-once call ledger.
`duration` is an optional `*time.Duration`; nil or negative omits the duration sample, never substitutes zero.
Observed interrupted calls can contribute cancelled/timeout counts with missing duration; do not fabricate success for calls without terminal evidence.
The producer owns event validation and when it has enough evidence to record; there are no per-call pairing maps or recovery of missed events.
Outcomes use the existing five bounded operation values.
Duration uses the existing ten finite operation buckets and omits outcome to avoid the histogram cross-product.

Layers are `mcp_server` (orchestrator admitted execution including gate waits and durable finalization), `mcp_client` (runtime-observed client waiting), and `native` (runtime native tools).
They are independent populations: server execution may outlive client waiting, and neither counts nor durations should be summed/merged across layers as one population.
Server recording reuses `mcp.tool.execute` completion without changing admission, authorization, validation, cancellation, detached mutation lifetime, or finalization; pre-admission rejections remain excluded.
Existing aggregate operation instruments remain intact.
`KnownMCPToolName(name)` checks the fixed 26-name MCP vocabulary, tested against the authorized catalog; native names are only `read`, `edit`, `write`, `bash`, `grep`, `find`, `ls`.
Unknown external/model-supplied names, layers, or outcomes are discarded, not normalized into new series.
No runtime producers are implemented here; #184 supplies client/native events through the existing live boundary.

### Mounted-storage sampling and runtime seam

| Instrument | Kind / unit | Dimensions | Meaning |
| --- | --- | --- | --- |
| `omnigrex.storage.observations` | Counter / `{observation}` | `resource`, `status` | Observed collection attempts, not a completeness ledger. |
| `omnigrex.storage.allocated` | Histogram / bytes | `resource`, `status` | Allocated directory blocks observed, including directory metadata; complete/partial samples only. |
| `omnigrex.storage.filesystem.capacity` | Histogram / bytes | None | Backing filesystem block capacity samples. |
| `omnigrex.storage.filesystem.free` | Histogram / bytes | None | Free blocks, including reserved blocks. |
| `omnigrex.storage.filesystem.available` | Histogram / bytes | None | Blocks available to an unprivileged caller. |
| `omnigrex.storage.filesystem.inodes` | Histogram / `{inode}` | None | Backing filesystem inode capacity samples. |
| `omnigrex.storage.filesystem.free_inodes` | Histogram / `{inode}` | None | Free inode samples. |

Storage histogram finite boundaries are `0, 1048576, 16777216, 268435456, 1073741824, 17179869184, 274877906944, 1099511627776`.
These broad shared buckets bound cost, not precise capacity quantiles; sum/count gives the mean sampled value, **not a current fleet total**.
Directory allocation and filesystem block use differ, especially for open-but-unlinked files.
Filesystem capacity is shared, not exclusive Participant capacity or a quota; never sum capacity samples across Participants or polls.

The background cache monitor initially retains its five-minute configured cadence.
Discovery unions configured workspace, runtime-state and mise backing roots, including idle retained Participant directories without starting an environment or invoking cleanup/inventory APIs.
Each backing root is discovered independently: a failed root records its resource categories as `unavailable` without discarding IDs from healthy roots or preventing later roots from being discovered.
Enumeration errors and non-`ENOENT` child-lookup failures also make discovery incomplete, while retaining IDs already discovered successfully; a readable but non-searchable root must not appear empty.
Absence-based pruning of retained scans and cache-warning state requires complete discovery; incomplete discovery does not establish that an undiscovered Participant was removed.
For both supplied Docker and Microsandbox mappings, exact `assignment-<Participant UUID>` directories map to `workspace`, `runtime-state`, `mise`, and `tool-data` under those roots.
The four persistent categories are `workspace`, `state`, `mise`, and `tool-data`; the latter includes assignment caches **and** Turn scratch, while mise covers the whole mounted installation tree.
No default path or Docker-daemon host path is used and no Participant is rebound.
Backend capacity is sampled from safely opened descriptors, once per device per poll, without Participant/category labels; independent polls and instances can still repeat samples.
A failed statfs omits capacity samples; it does not become zero free space.

The scanner reuses metadata-only descriptor-relative no-follow traversal, ownership/device checks, bounded depth/hardlink tracking, 500,000-entry steps, 30-second per-Participant budgets, and at most four retained mounted-tree scans in addition to the independent four cache-warning scans.
Discovery retains the existing 2,000-Participant rotating poll budget.
It never reads file contents or exports filenames, repairs permissions, pauses agents, captures workspaces, reclaims leases, or performs retention/eviction.
Filesystem syscalls cannot be preempted by context cancellation; observations remain off the Agent Turn critical path.
Existing retained tool-cache threshold/suppression semantics and scratch/retention cleanup remain independent and unchanged.

Statuses are `complete`, `partial`, `missing`, or `unavailable` (including inaccessible/unsafe roots and exhausted concurrent-scan capacity).
A completed empty tree contributes an actual allocation sample (directory metadata may use blocks); missing/unavailable observations contribute **no usage sample**.
Partial samples are cumulative progress from a resumable traversal and can overlap across polls; do not sum them.
Completed concurrent traversals are neither atomic snapshots nor necessarily lower bounds at completion; replacement invalidates retained scan state on the next root check.
There is no exact fleet-total, collection-percentage, per-Turn peak/growth, retained measurement ledger, or measurement recovery protocol.
There is no per-owner observation-age gauge: the status counter indicates sampled collections but cannot establish freshness of every retained Participant; collection/export absence must remain unknown.

The runtime companion may call `RecordStorage(ctx, resource, status, allocated)` for fixed `tmp` and `run-pi-agent` categories only; host scans never measure those temporary mounts.
Nil allocation is required when unavailable/missing; only observed complete/partial values may be supplied, including actual zero.
`RecordFilesystem` accepts optional nonnegative finite numeric samples, independently omitting unavailable values; producers must avoid duplicate shared-capacity samples within one collection.
There is no universal observation abstraction or new Agent Event schema until a producer needs it.
VM-root space including `/var/tmp`, host-only Workspace Captures/native log archives, and whole-host monitoring are excluded.

### Revised bounded series budget

The existing 653 application series remain compatible.
This extension adds at most **1299** classic series: 59 authorized layer/tool pairs × (5 counters + 13 histogram series) = 1062; two prompt boundaries × 13 = 26; six resources × four statuses = 24; six resources × two usable statuses × 11 allocation histogram series = 132; five filesystem histograms × 11 = 55.
The combined application ceiling is **1952**, with a reviewed **2050 total target** allowing 98 backend metadata series, replacing the preceding 750 target.
Executable tests enumerate all combinations and exercise SDK view/filter retention; arbitrary labels remain disallowed.
Resource identity churn and backend translation can multiply retained series and must be inspected during deployment.

Deployment dashboards and authorized live verification remain #185, after runtime producers in #184.
Inspect actual units/names/buckets/metadata, distinguish layers and partial/missing storage, and verify stdout log/trace navigation without adding labels or another logging route.
Local code tests and any existing received signals do not establish live backend acceptance, dashboard correctness, or notification activation.
