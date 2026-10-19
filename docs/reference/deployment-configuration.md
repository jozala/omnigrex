# Deployment configuration reference

[Reference](README.md) · [Operator guide](../operators/README.md)

## Configuration layers

The no-clone stack uses [deploy/.env.pi.example](../../deploy/.env.pi.example), a common Compose file, exactly one backend overlay, and explicit overrides.
The source-built stack uses [.env.example](../../.env.example) and root [compose.yaml](../../compose.yaml).
Exported shell variables take precedence over `.env` during Compose interpolation.
An `.env` value reaches the process only when the Compose service forwards it; add an explicit service environment override for a binary setting not forwarded by the selected bundle.
Keep the exact `COMPOSE_FILE` exported for every operational shell.

The binary validates settings in [internal/config/config.go](../../internal/config/config.go).
Compose-only image, port, secret-source, group, and healthcheck settings are interpreted by the bundle rather than that loader.
Do not assume an unset binary default equals the value supplied by Compose.

## Images and authority

| Setting | Meaning |
| --- | --- |
| `OMNIGREX_ORCHESTRATOR_IMAGE` | Exact qualified selected-backend controller digest for no-clone deployment |
| `OMNIGREX_PI_IMAGE`, `OMNIGREX_PI_PLATFORM` | Required exact golden registry digest and supported Linux platform |
| `OMNIGREX_EXECUTION_BACKEND` | One installation backend, `docker` or `microsandbox`; selected by overlay |
| `OMNIGREX_DOCKER_GID` | Docker socket group, Docker only |
| `OMNIGREX_MICROSANDBOX_KVM_GID` | KVM device group, native overlay only |
| `OMNIGREX_SECRET_GID` | Dedicated secret-file group distinct from backend authority group |
| `OMNIGREX_GITHUB_DEVELOPER_APP_ID`, `OMNIGREX_GITHUB_REVIEWER_APP_ID` | Distinct numeric App identities |
| `OMNIGREX_RUNTIME_PROFILE_COMPATIBILITY_RESULTS_FILE` | Optional private absolute host path mounted for exact backend-scoped qualification results |

See [release/image selection](../operators/installation.md), [App permissions](../operators/github-apps.md), and [secret placement](../operators/secrets.md).
`OMNIGREX_DATABASE_PASSWORD_FILE`, App private-key files, webhook-secret file, and provider-credentials file identify Compose host secret sources.
The controller reads mounted secrets, including `OMNIGREX_DATABASE_PASSWORD_SECRET_FILE` for its PostgreSQL password path.
Never put secret contents into examples or command-line values.

## Runtime and maintenance budgets

| Setting | Supplied stack value or binary default |
| --- | --- |
| `OMNIGREX_AGENT_TURN_CONCURRENCY_LIMIT` | 2 |
| `OMNIGREX_AGENT_TURN_MEMORY_MIB` | Compose 1024 MiB; unset binary 512 MiB |
| `OMNIGREX_AGENT_TURN_EXECUTION_TURN_TIMEOUT` | `2h` |
| `OMNIGREX_PI_STARTUP_TIMEOUT` | `10m` |
| `OMNIGREX_AGENT_TURN_EXECUTION_CLEANUP_TIMEOUT` | `10s` |
| `OMNIGREX_TERMINAL_CORROBORATION_DURATION` | `30m` |
| `OMNIGREX_ASSIGNMENT_RETENTION_DURATION` | `720h` |
| `OMNIGREX_ASSIGNMENT_TOOL_CACHE_WARNING_MIB` | 1024 MiB, soft warning only |
| `OMNIGREX_ASSIGNMENT_TOOL_CACHE_POLL_INTERVAL` | `5m` |
| `OMNIGREX_READINESS_TIMEOUT` | Compose `60s`; unset binary `15s` |
| `OMNIGREX_HEALTHCHECK_TIMEOUT` | Compose `70s`, at least 10s beyond readiness |
| `OMNIGREX_SHUTDOWN_TIMEOUT` | `10s` |
| `OMNIGREX_STOP_GRACE_PERIOD` | Compose `1m`; must cover draining/cleanup plus final telemetry shutdown |

Startup and whole-Turn deadlines overlap; increasing one does not automatically increase the other.
See [installation sizing](../operators/installation.md#5-size-the-installation), [health](../operators/health-and-diagnostics.md), and [retention](../operators/retention.md).

## Policy, networking, and storage

| Setting | Purpose |
| --- | --- |
| `OMNIGREX_AGENT_INSTRUCTIONS_DIR` | Optional mounted operator guidance loaded at controller startup |
| `OMNIGREX_AGENT_PATH_ENV_ALLOWLIST` | Approved comma-separated repository path-variable names |
| `OMNIGREX_HTTP_PORT` | Compose published HTTP port/address, default 8080 |
| `OMNIGREX_HTTP_ADDR` | Binary HTTP listener, default `:8080` |
| `OMNIGREX_MCP_ADDR`, `OMNIGREX_MCP_ENDPOINT_URL` | Private MCP listener/address contract; never expose publicly |
| `OMNIGREX_WORKSPACE_VOLUME`, `OMNIGREX_RUNTIME_STATE_VOLUME`, `OMNIGREX_MISE_VOLUME` | Logical Docker storage names, must match actual overridden volumes |
| `OMNIGREX_DOCKER_AGENT_NETWORK` | Private agent network, must match the selected Compose network |
| `OMNIGREX_WORKSPACE_ROOT`, `OMNIGREX_RUNTIME_STATE_ROOT`, `OMNIGREX_WORKSPACE_CAPTURE_ROOT`, `OMNIGREX_MISE_ROOT` | Control-plane storage roots with protected binding/ownership requirements |

Some advanced settings require Compose environment/mount overrides; use the loader and selected bundle together as the exact contract.
Backend and namespace bindings are immutable after initialization.
Changing only a Compose project name is insufficient to isolate a second installation.
See [agent policy](../operators/agent-policy.md), [native backend settings](../operators/backends.md#native-configuration-and-storage), and [installation](../operators/installation.md#2-stage-deployment-files).

## Telemetry and qualification

[Observability](../operators/observability.md) owns OTLP endpoints, headers, samplers, metrics opt-in, and export timing.
[Runtime qualification](../contributors/runtime-qualification.md) owns `OMNIGREX_PI_*` fixture/upgrade/live-gate inputs.
In particular, `OMNIGREX_PI_DEPLOY_ORCHESTRATOR_IMAGE` and `OMNIGREX_PI_DEPLOY_IMAGE` select test script images, not normal deployment images.
Do not place qualification credentials or compatibility output in source control.
