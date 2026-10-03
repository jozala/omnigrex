# Diagnosing ACP failures

ACP connects the orchestrator to the Runtime Process. MCP connects the agent to
Omnigrex capabilities. MCP tool failure logs do not cover ACP requests.

ACP operational logs use the orchestrator's logger. In deployments that collect
it under `service_name="omnigrex"`, search for:

```logql
{service_name="omnigrex"} | json | msg="ACP request failed"
```

To identify unsupported requests:

```logql
{service_name="omnigrex"} | json | rpc_code=-32601
```

## Request failures

`ACP request failed` records contain:

- `method`, `request_id`, and `duration_ms`.
- `direction`: `agent_to_orchestrator` means the orchestrator handled a callback;
  `orchestrator_to_agent` means the orchestrator called the Runtime Process.
- `failure_stage`: `submit`, `response`, `decode`, or `validate` for outgoing calls;
  `handler` or `write` for incoming callbacks.
- `failure_class` and `rpc_code` when an RPC error is available.
- Workflow, Assignment, Agent Session, Agent Turn, and execution epoch context
  for launched Runtime Processes, including initialization failures. ACP Session
  identity is included when available.

`-32601` at `handler` identifies a method rejected by the orchestrator.
`-32601` at `response` identifies a method rejected by the Runtime Process.
A logged handler failure records the decision to return an error; a subsequent
`write` failure means delivering the response also failed.

Initialization validation failures use `method="initialize"` and
`failure_stage="validate"`. The failure class distinguishes
`protocol_version_mismatch` from `required_capability_missing`; these warnings
retain the request's original ID, timing, and Agent Turn correlation.

Other per-response validation failures also use `failure_stage="validate"`:
`unknown_stop_reason` for `session/prompt`, `empty_session_id` for `session/new`
or `session/list`, and `session_discovery_limit` for an oversized `session/list`
page. Unknown stop-reason values and invalid response bodies are not logged.
Cross-page recovery-policy failures are not attributed to an individual RPC.

## Other failures and lifecycle events

- `ACP notification failed`: an outgoing notification could not be submitted.
- `ACP protocol failed`: an incoming frame was malformed or invalid. A malformed
  frame may not provide a usable method or request ID. Valid method and scalar ID
  fields are recovered for diagnostics even when other envelope fields have
  invalid types; this does not change the protocol rejection response.
- `ACP connection closed`: includes the failure stage and pending request count.
  Unexpected EOF is a warning; explicit close is informational. Pending calls
  also log their methods individually when they fail.
  If a prompt does not finish within its cancellation grace period, the close
  logs `failure_stage="cancellation_grace"` and
  `failure_class="deadline_exceeded"` as a warning. The pending prompt's failure
  warning retains its original request ID, Agent Turn context, and duration.

Cancellation is informational; deadline expiry and unexpected failures are
warnings. Successful requests do not produce failure logs. A failure log alone
does not establish the Workflow outcome: correlate it with Agent Turn outcome
reconciliation logs.

Logs deliberately exclude prompts, tool arguments, request/response bodies,
remote error messages/data, raw frames, and arbitrary local error strings. A
failure class and stage narrow the cause without exposing those payloads.

These logs cannot diagnose an internal Runtime Process connection that never
crosses the orchestrator's ACP transport. If runtime stderr reports an error
without a corresponding ACP entry, inspect runtime-side diagnostics too.

For deployment and recovery procedures, see the [operator guide](../operator-guide.md).
