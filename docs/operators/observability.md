# Configure logs, traces, and application metrics

[Operator guide](README.md) · [Troubleshooting](troubleshooting.md)

Logs are JSON on stdout and use your existing Docker collection route.
Preserve JSON fields and keep domain IDs, trace/span IDs, diagnostic call IDs, and GitHub request IDs out of log stream labels.
There is no direct OpenTelemetry log export or public metrics endpoint.

## Provider and subscription request failures

`Agent Session observation` records with `kind=provider_request_failed` describe an observed failed model request, including native compaction and branch-summary requests; `kind=prompt_settled` records why the Agent Turn ended.
These WARN records use the existing controller stdout logging pipeline and survive Runtime Process cleanup.
Join them by `workflow_id`, `agent_participant_id`, `agent_session_id`, `agent_turn_id`, `execution_epoch`, and trace/span fields when available.
`retry_of_turn_id` links a retried Turn to the earlier failed or interrupted Turn.
The `runtime` field is a JSON object, with `selected_model` (the configured provider/model) and `provider_failure`:

| Field | Meaning |
| --- | --- |
| `attempt` | Locally generated UUID for one model request, not a provider ID; absent when unavailable. |
| `category` | Controller-validated observed subscription category (`subscription_quota`, `subscription_auth`, `subscription_configuration`, or `subscription_temporary`); absent when unavailable. Diagnostic evidence, not a retry-policy decision. |
| `auth_mode` | `subscription` for the observed controller-managed path, otherwise `unknown`. |
| `stage` | `credential`, `payload`, `provider_http`, `provider_stream`, `transport`, `local`, or `unknown`. |
| `cause` | `http`, `stream_error`, `network`, `timeout`, `cancelled`, `validation`, `protocol`, or `unknown`. |
| `http_status` | Observed status or null. At `credential` this is the **controller auth endpoint**, not OpenAI. At `provider_stream`, 200 describes the initial successful response, not successful inference. |
| `code` | Explicitly allowlisted structured provider code, or `unknown`; arbitrary codes/types are discarded. |
| `retryability` | `yes`, `no`, or `unknown`; currently unknown unless explicitly observed. The broad failure category does not establish retryability. |
| `elapsed_ms` | Bounded time from request start to terminal observation, including credential resolution; null if unavailable. |
| `request_id_hash` | SHA-256 fingerprint of a bounded `x-request-id`, absent when unavailable or invalid; never the raw header. |

`subscription_temporary` is a fallback classification, **not proof of a provider outage or an OpenAI error response**.
For example, `stage=transport, cause=network, http_status=null` establishes no HTTP response; `stage=provider_http, http_status=502, code=unknown` does establish a provider HTTP error despite an unknown code.
`stage=provider_stream, http_status=200, cause=stream_error` identifies a later structured SSE error; a stream read failure can instead carry `network`, `timeout`, or `unknown`.
`payload/validation` is local request preparation, while `provider_stream/protocol` can indicate an unexpected non-SSE response.
A genuinely unobserved local/SDK cause remains unknown rather than being inferred from error prose.
Native summary requests emit their own bounded request observation even though they do not produce conversational assistant messages.
A generic `Omnigrex compaction_failed` lifecycle marker is not the only evidence: search the same Turn for `provider_request_failed` and its attempt UUID.
For native summary failures, `runtime.provider_failure.category` survives into the settlement diagnostic and Human Handoff text, while the lifecycle failure class remains `provider_failure`.
For example, a failed summary can report `provider_failure: model request failed (category=subscription_temporary stage=provider_http cause=http http_status=502 ...)`, not a runtime admission rejection.
Observed quota/auth/configuration categories on summary requests do not by themselves reroute settlement into a prerequisite failure or change retries.
An admitted aborted outcome with observed `cause=timeout` retains that cause in settlement and Human Handoff diagnostics; its interrupted status and existing retry policy are unchanged.
Other providers currently retain unknown observations through the shared boundary; do not infer subscription evidence for them.

For JSON controller logs collected into a file:

```sh
jq 'select(.agent_turn_id == "TURN-ID" and (.kind == "provider_request_failed" or .kind == "prompt_settled")) | {workflow_id, agent_turn_id, execution_epoch, retry_of_turn_id, failure_class, runtime, trace_id}' controller.jsonl
jq 'select(.workflow_id == "WORKFLOW-ID" and .msg == "Agent Turn infrastructure retry decision") | {agent_turn_id, retry_of_turn_id, retry_budget_used, retry_budget_limit, reason}' controller.jsonl
```

In Grafana/Loki, select your existing controller stream and filter parsed fields, for example `{service_name="omnigrex"} | json | agent_turn_id="TURN-ID"` (adapt the stream label to your collector).
Do not make Turn IDs, attempt UUIDs, fingerprints, or other correlation values stream/metric labels.
Start with the Turn reference in the Human Handoff, then search `retry_of_turn_id` to find the interruption or failure that consumed the automatic retry budget.
`Agent Turn infrastructure retry decision` records the used/limit budget after a committed live settlement and its fixed decision reason.
Retry-exhaustion Human Handoffs also include used/limit and both Turn references; recovery-owned interruptions may have no provider request observation, so follow their Turn and recovery logs instead of expecting a provider ID.
This diagnostic change does not increase retry limits, automatically resume a Workflow, or clear subscription blocks.
Use the existing [authentication recovery procedure](provider-authentication.md#recover-access) for credential problems and explicitly resume only after resolving the underlying issue.

Raw response bodies, arbitrary exception text/stacks, prompts, completions, tool payloads, credentials, cookie/authorization headers, and URLs/query strings are never part of this schema.
Do not enable unrestricted SDK/HTTP debug logging or reproduce a paid request merely to obtain basic failure evidence.
The original request ID cannot be recovered from its fingerprint; separately obtained evidence can be hashed for comparison.

## Microsandbox SDK failures

`Microsandbox SDK call failed` records the failing `sdk_operation`, `sdk_error_kind`, original `error`, and `duration_ms`.
Available resource name/ID/status and `agent_participant_id`, `agent_session_id`, `agent_turn_id`, and `execution_epoch` identify the execution; trace/span fields follow the calling context.
Unexpected SDK failures are ERROR; cancellation/deadline failures are WARN.
Successful calls and expected absent-sandbox lookups do not produce these records.
Native handle close/detach timeouts report an unresolved operation, not proof of completion.
The timeout record has `sdk_failure_phase=release_wait`; if native work later returns a concrete SDK error, that original cause is logged separately without repeating close/detach or extending the wait.
Failed-exec and stdin-error events are logged even when the SDK receive call returns no Go error, with `sdk_failure_event` and available path/errno details.

SDK error text is intentionally **not redacted** and can contain sensitive data, guest output, or configuration.
Restrict access to these operator logs and inspect them before sharing.
A TODO at the logging boundary tracks future sensitive-data prevention; returned errors remain sanitized for their existing callers.
These JSON records reach Loki/Grafana through the existing Docker log collector, not a new exporter.
Complete [native runtime log files](troubleshooting.md#retained-microsandbox-native-logs) remain host-local and are not forwarded to Grafana.

## Tracing

Tracing uses OTLP/HTTP with protobuf encoding and is inactive unless a generic or trace-specific endpoint is configured.
Both Compose stacks pass tracing settings from private deployment configuration.
For example:

```dotenv
OTEL_EXPORTER_OTLP_ENDPOINT=http://collector.example:4318
OTEL_SERVICE_NAME=omnigrex
```

The generic endpoint appends `/v1/traces`; `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` is a complete URL and takes precedence.
Use the collector's HTTP endpoint, not its gRPC endpoint.
`OTEL_EXPORTER_OTLP_PROTOCOL` is unnecessary and does not select another exporter.
If authentication is needed, use URL-encoded `key=value` pairs in `OTEL_EXPORTER_OTLP_HEADERS`; `OTEL_EXPORTER_OTLP_TRACES_HEADERS` overrides them for traces.
Treat authentication headers as [private credentials](secrets.md).
Recreate the controller after changing environment settings.

The collector must listen on a reachable interface and connect its receiver to an export pipeline.
Container loopback refers to the container, not the Docker host.
Docker Desktop supplies `host.docker.internal`; Linux needs a host-gateway mapping in an override or another reachable host IP.

Service identity defaults to `omnigrex` and includes the build version.
Published builds identify their commit; unversioned local builds use `dev`.
Override identity through `OTEL_SERVICE_NAME` and `OTEL_RESOURCE_ATTRIBUTES`.
The default sampler is `parentbased_always_on`.
Use `always_on` to sample all traces, or `parentbased_traceidratio` with `OTEL_TRACES_SAMPLER_ARG=0.1` for 10% of new roots while respecting parents.

### Correlate operations

Instrumentation covers webhook/MCP HTTP requests and outbound GitHub calls with W3C trace-context and baggage propagation.
Health/readiness HTTP requests and Docker readiness/image-availability probes suppress tracing; operational Docker requests retain it.
Request bodies, tool arguments, and prompts are not trace attributes.

Claimed preparation, execution, webhook processing, mutation recovery, cleanup, outcome reconciliation, and admitted MCP tools use fixed operation names.
Launch and reconciliation normally nest under execution, and clients inherit the calling context.
Deferred corroboration creates another reconciliation attempt.
Admitted MCP mutations can outlive HTTP disconnection, and a tool error can occur despite HTTP 200.
Expected blocked outcomes differ from infrastructure failures.

Durable workers start independent traces; context is not persisted through queues or propagated across the Runtime Process MCP boundary.
Join investigations through `workflow_id`, `agent_participant_id`, `agent_session_id`, and `agent_turn_id` fields.
Context-aware JSON logs include `trace_id` and `span_id`, even for unsampled contexts.
A trace link may have no stored trace because of sampling or delivery loss.
Long execution spans export on completion; start logs and shorter child spans provide earlier visibility.

## Application metrics

Application metrics are independently opt-in and use OTLP/HTTP protobuf.
Both Compose stacks accept the same metrics settings.

| Configuration | Traces | Metrics |
| --- | --- | --- |
| No endpoints | Disabled | Disabled |
| Generic endpoint only | Enabled | Disabled |
| Trace endpoint only | Enabled | Disabled |
| Metrics endpoint only | Disabled | Enabled |
| Generic endpoint plus `OTEL_METRICS_EXPORTER=otlp` | Enabled | Enabled |
| Trace and metrics endpoints | Enabled | Enabled |
| `OTEL_METRICS_EXPORTER=none` | Existing endpoint-based behavior | Disabled |
| `OTEL_SDK_DISABLED=true` | Disabled | Disabled |

`otlp` requires a generic or metrics endpoint; unsupported exporter selections fail startup.
The generic endpoint appends `/v1/metrics`, while `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` is a complete URL and takes precedence.
`OTEL_EXPORTER_OTLP_METRICS_HEADERS` overrides generic headers using the same URL-encoded format.
Use HTTP(S) and header authentication rather than URL credentials.

`OTEL_METRIC_EXPORT_INTERVAL` defaults to 60000 milliseconds and `OTEL_METRIC_EXPORT_TIMEOUT` to 30000.
Compose sets `OTEL_EXPORTER_OTLP_METRICS_TIMEOUT=10000` milliseconds.
Timing values must be positive integers.
A longer interval reduces samples rather than series cardinality.

Traces and metrics share build/resource identity.
SDK views exclude unplanned library metrics and non-allowlisted application dimensions.
Durable gauges use one read-only snapshot with a five-second deadline, including connection acquisition, and a four-second statement timeout.
Observation failure omits all durable gauges and increments `omnigrex.observation.failures`; it never emits substitute zeros or cached state.
Treat missing observations as unavailable, not an empty backlog.

## Shutdown and delivery

Completed spans are batched and flushed after services stop.
Trace and metrics shutdown share a fresh `OMNIGREX_SHUTDOWN_TIMEOUT` budget; observation registration and database remain open until export finishes.
Compose defaults to a one-minute stop grace via `OMNIGREX_STOP_GRACE_PERIOD`.
Keep it longer than service draining and Turn cleanup plus a separate telemetry shutdown allowance.
Increase it when those budgets grow.
Exporter failures report safe categories without stopping Workflow processing.

### Telemetry SDK diagnostics

`telemetry SDK failure` WARN records use the controller's existing local JSON stdout route, not telemetry export.
They remain available when the collector is unreachable; no debug payload logging is required.

| Field | Meaning |
| --- | --- |
| `signal` | `metrics`, `traces`, or explicitly `unknown`. |
| `stage` | `collection` at the durable-state observation callback, `export` at the signal-specific exporter boundary, `response` for a typed partial-success response, `shutdown` at provider shutdown, or `unknown` for remaining SDK reports. |
| `error_kind` | `eof`, `unexpected_eof`, `connection_reset`, `connection_refused`, `timeout`, `cancelled`, `partial_success`, or `unknown`, derived from typed evidence rather than error prose. |
| `rejected_items` | Nonnegative integer from the pinned SDK's typed partial-success response; absent otherwise. |

For example, `signal=metrics, stage=export, error_kind=eof` establishes a metrics export that ended in EOF, not the reason the peer disconnected.
It does not establish an idle-timeout race or change exporter retries.
`error_kind=partial_success, stage=response` describes an HTTP success response with partial rejection or a server warning, not a failed export; `rejected_items` can be zero when only a warning was returned.
The arbitrary server message is discarded.
Durable-state observation failures still omit gauges and increment the existing failure counter; the local collection diagnostic adds no telemetry instruments.

The pinned Go HTTP exporters do not expose typed HTTP rejection status to this boundary.
Such failures retain their known signal and `stage=export`, but `error_kind=unknown`; no `http_status` is emitted or inferred from the SDK's secret-bearing formatted error.
Malformed successful responses also remain unknown unless a typed underlying cause survives.
Other SDK collection callbacks may fail before export without retaining signal/stage context; their global record explicitly remains unknown, never an assumed delivery failure.
Partial-success extraction is pinned to the exact SDK types and safely degrades to unknown if their shape changes.

A failed export may have both a signal-aware boundary record and an unknown global SDK record, and a shutdown flush may also report a shutdown record.
These are not independent failed requests or a reliable loss counter; do not correlate unknown reports to a signal solely by timing.
No URLs, headers, raw errors/stacks, response bodies, or telemetry content are included.
Keep these fields out of metric/stream labels and use parsed JSON log queries instead.

To investigate, select the existing controller log stream and filter `msg="telemetry SDK failure"`, then examine `signal`, `stage`, and `error_kind`.
Check collector reachability and independently available collector diagnostics for unknown exports; do not enable unrestricted HTTP/SDK logging to recover status or remote text.
After correcting the underlying configuration or collector problem, verify subsequent samples/spans arrive using the existing backend, since absence of warnings alone does not prove delivery.
These records do not stop Workflows, change Agent Turn outcomes, or automatically recover missing samples.

## Dashboards and alerts

Use the [application observability contract](../observability/application.md) for instrument names, queue eligibility, semantics, series budget, dashboard/alert requirements, and pending live verification.
Its application series target is bounded; inspect actual backend translation and metadata before accepting a deployment.
Configure log/trace navigation, no-data behavior, thresholds, and notifications in the deployment's own records.
Application instrumentation tests do not establish successful live collector delivery or alert behavior.

## Diagnose an Agent Turn step budget

Use the correlated per-Turn observations and settlement outcome described in the [step-budget contract](../reference/agent-step-budgets.md#per-turn-diagnostics).
Summary-only entry is pressure, not a failure verdict.
Aggregate step-budget instruments are deferred to Work Item #112.
