# Configure logs, traces, and application metrics

[Operator guide](README.md) · [Troubleshooting](troubleshooting.md)

Logs are JSON on stdout and use your existing Docker collection route.
Preserve JSON fields and keep domain IDs, trace/span IDs, diagnostic call IDs, and GitHub request IDs out of log stream labels.
There is no direct OpenTelemetry log export or public metrics endpoint.

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

## Dashboards and alerts

Use the [application observability contract](../observability/application.md) for instrument names, queue eligibility, semantics, series budget, dashboard/alert requirements, and pending live verification.
Its application series target is bounded; inspect actual backend translation and metadata before accepting a deployment.
Configure log/trace navigation, no-data behavior, thresholds, and notifications in the deployment's own records.
Application instrumentation tests do not establish successful live collector delivery or alert behavior.
