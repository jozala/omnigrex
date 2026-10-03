# MCP failure diagnostics improvement plan

## Goal

Make future MCP read failures and shared gateway rejections answer:

- **Where did the call fail?** Authorization, arguments, credentials, GitHub request, decoding, validation, result validation, or ledger recording.
- **Which check failed?** A specific missing field, identity mismatch, invalid location, or pagination inconsistency.
- **Did GitHub return a response?** Actual HTTP status and validated request ID for the failing operation, when available.
- **Which failures belong to the same invocation?** Including a backend failure followed by a recording failure.

Keep credentials, tool arguments, GitHub content, and raw error messages out of logs.
All file paths below are relative to the repository root.

The in-progress ACP logging work remains separate.
It improves transport diagnostics but does not capture MCP backend failures.
Implementation must preserve those existing changes through narrowly scoped edits.

## Scope and boundaries

This change covers read-tool backend failures, authenticated shared gateway rejection paths, and safe ACP failure summaries.
Precise GitHub validation instrumentation initially covers review-thread reads and issue lookup.
Other read validators can adopt the same model later.

Mutation-specific signing, serialization waiting, planning, reservation, replay, execution, and finalization diagnostics are not expanded in this change.
Shared gateway rejection logging applies to mutation requests before dispatch to mutation handling.
Shared GitHub and MCP helper changes must preserve existing mutation classification, durable diagnostics, and retry behavior.
Do not claim that this change diagnoses every mutation failure.

Runtime-local failures that never reach the gateway and requests for which trusted registration scope is unavailable remain diagnostic blind spots.
No database schema changes are required.

## 1. Define a structured, safe diagnostic model

**Files:** `internal/github/errors.go`, `internal/mcp/failure.go`

Extend the existing typed-error approach rather than logging `err.Error()`.

| Field | Meaning |
|---|---|
| `failure_code` | Broad classification, such as `github_invalid_response` |
| `failure_stage` | Most specific known processing stage, such as `review_threads_validation` or `read_recording` |
| `failure_reason` | Fixed failed-rule identifier, such as `missing_field` or `duplicate_id` |
| `validation_field` | Code-defined schema field path, such as `review_thread.originalLine` |
| `github_http_status` | Actual status from the response associated with the failure, omitted if unknown |
| `github_request_id` | Request ID from that same response, passing the existing strict validation |
| `diagnostic_call_id` | Gateway-generated request identity shared by logs for one invocation |
| `duration_ms` | Elapsed gateway processing time at the moment of the log event |

Retain existing Workflow, Agent Turn, execution epoch, and resolved tool-name fields.
Omit unavailable optional metadata rather than emitting empty strings or status `0`.
Unknown classifications use a fixed fallback such as `unclassified`, never raw error text.

### Safety and compatibility rules

- Reasons and field paths come from code-defined allowlists, never response values or dynamic array indexes.
- Enforce allowlists when diagnostics cross the GitHub-to-backend and backend-to-log boundaries; string-based Go enum types alone are insufficient validation.
- Validate HTTP status values and apply the existing strict GitHub request-ID allowlist.
- Typed validation errors continue matching `ErrInvalidAPIResponse` through `errors.Is`.
- Preserve existing sentinel matching, wrapping contracts, retry behavior, and generic agent-facing messages.
- Do not expose raw causes through newly exported diagnostic metadata.
- Do not treat MCP request IDs as ACP tool-call IDs; those identities are not guaranteed to match.

### Classification precedence

Classify from evidence in the returned error or its safe diagnostic representation, not solely from the current request-context state.
Use this precedence when evidence overlaps:

1. Cancellation or deadline expiry explicitly represented by the returned failure.
2. Typed HTTP rejection evidence.
3. GraphQL envelope errors or missing data, even though they also match `ErrInvalidAPIResponse`.
4. Response decoding or validation failure.
5. Transport failure.
6. Other dependency failures or a fixed unclassified fallback.

A successful HTTP status is response metadata, not rejection evidence.
Preserve an operation-specific stage when classifying cancellation or another cross-cutting failure.
Use distinct stages for credential acquisition, HTTP execution, response decoding, GraphQL data decoding, scoped identity validation, backend result validation, and read recording where no more specific existing stage applies.

## 2. Identify exact GitHub validation failures

**Files:** `internal/github/api.go`, `internal/github/errors.go`, `internal/mcp/backend.go`

### Review-thread reads: first priority

Split compound validation expressions into ordered checks returning safe typed diagnostics.

| Area | Checks to distinguish |
|---|---|
| Repository/PR identity | Missing object or field, invalid node ID, requested identity mismatch, identity changed between pages |
| Thread connection | Missing connection/count/nodes, invalid total count, oversized page |
| Thread fields | Missing resolution/outdated flags, invalid repository path |
| Thread location | Invalid diff side or subject type, missing/invalid original line, inconsistent current line, invalid multiline range |
| Comment fields | Missing identity, body, author, commit, timestamp, or invalid URL |
| Comment location | Subject/path mismatch, incompatible line information |
| Uniqueness | Duplicate thread ID, comment node ID, or comment database ID |
| Pagination | Missing page info/cursor, repeated cursor, changing count, final count mismatch |
| Replies | Invalid reply target, missing target, first comment is a reply, reply does not reference thread root |
| Continuation response | Changed thread identity/location, changed comment count, mismatched PR identity |

For compound continuation checks, preserve the underlying validator's precise reason instead of replacing it with another generic error.
For complicated location validators, provide an error-returning validator that identifies the failed rule.
Avoid a separate diagnostic implementation that can drift from the acceptance rules.
The tuple of stage, reason, and field must distinguish different location rules even when they concern the same field.

This change must explain existing validation behavior without relaxing or tightening it.
Preserve validation order, short-circuit behavior, and the first failing check when several defects are present.

### Issue lookup: second priority

Instrument `GetIssue` checks individually:

- Invalid or missing issue ID or node ID.
- Issue number mismatch.
- Empty title.
- Invalid state.
- Invalid or mismatched issue URL.

Distinguish the backend's scoped issue identity mismatch from a generic read precondition failure while preserving `ErrToolPrecondition` matching.
This validation happens after `GetIssue` successfully returns, whose interface does not carry response metadata.
For this change, report a precise `scoped_identity_validation` stage and reason but omit GitHub status and request ID for that backend-only mismatch.
Do not infer status `200`, attach an unrelated previous response, or add mutable client-wide last-response state.
Successful-result metadata propagation across backend interfaces is outside this change.

## 3. Preserve GitHub response metadata

**Files:** `internal/github/api.go`, `internal/github/errors.go`, `internal/mcp/failure.go`

Introduce an internal response-metadata carrier preserving the actual HTTP status and GitHub request ID.
Capture metadata as soon as a response is available, before decoding its body.

Propagate it through:

1. HTTP-response decoding errors.
2. GraphQL envelope errors.
3. GraphQL data decoding errors.
4. Post-response validation errors within the GitHub client.
5. Review-thread pagination and continuation errors.
6. Issue lookup validation errors within the GitHub client.

### Metadata provenance

- Status and request ID must originate from the same response.
- A continuation failure uses its own response metadata, not an earlier page's request ID.
- A failing request that received no response must not inherit metadata from an earlier successful request.
- A cross-page identity/count inconsistency uses the response that exposed the inconsistency.
- A final aggregate count mismatch uses the last contributing response.
- Missing or rejected request IDs remain absent even when an earlier response supplied a valid ID.

### Classification and shared-helper compatibility

- A validation failure after HTTP `200` remains `github_invalid_response`.
- An HTTP rejection remains `github_request_rejected`.
- Do not classify every nonzero status as a rejection.
- Keep successful-response metadata extraction distinct from typed rejection detection.
- Audit every consumer of shared metadata helpers, including `githubMutationFailure` and `githubObservationFailure`.
- Preserve existing mutation-facing messages and durable diagnostic formats.
- Keep redaction from discarding typed diagnostics, sentinel matching, or retry metadata.

Distinguish these GraphQL failures without recording GitHub messages:

- Response contains GraphQL errors.
- Response has no data.
- Response contains undecodable data.

## 4. Preserve cancellation before sanitization

**Files:** `internal/github/api.go`, `internal/github/errors.go`, `internal/mcp/backend.go`, `internal/mcp/failure.go`

Classify cancellation and deadline expiry before sanitization or generic dependency conversion discards the cause.
Checking only the gateway's final error chain is insufficient.

Cover:

- Credential acquisition failures before `credential()` collapses them into `ErrToolDependency`.
- HTTP execution failures before `newGitHubReadError` discards the cause.
- Cancellation or deadline expiry during response-body reading, whose decoder errors currently lose identity through `%v` formatting.

Retain safe cancellation classification and the processing stage without exposing raw causes.
Do not reclassify an independent validation failure merely because the request context was subsequently canceled.
Preserve existing retry decisions; richer diagnostics must not silently alter retry eligibility.

## 5. Log shared gateway failures before backend execution

**File:** `internal/mcp/gateway.go`

Add `MCP request rejected` logs for authenticated shared gateway rejection paths:

- Invalid request envelope or protocol state.
- Unsupported path, method, media type, or Accept requirements where the existing handler rejects the authenticated request.
- Invalid call parameters or metadata.
- Unknown or disallowed tool.
- Invalid tool arguments.
- Missing tool-specific turn context.
- Authorization becoming stale or closed after authentication.
- Failed turn-fence validation.
- Failed read admission.

Use fixed stages such as `request_validation`, `initialization`, `tool_authorization`, `argument_validation`, `turn_fence`, and `admission`.
Split combined checks where needed without changing their evaluation order or response behavior.
Distinguish a fence-validation dependency failure from proven stale authorization when existing typed errors permit it.
Do not label accepted notifications or expected non-error protocol behavior as failures.

### Trusted scope and authentication boundary

Log tool names only after resolving them to known tool definitions.
Never log arbitrary submitted tool names, JSON-RPC IDs, methods, paths, arguments, headers, or bodies.
Use trusted registration scope for correlation.

`authenticate()` currently rejects non-live registrations without returning trusted scope.
This change preserves that boundary: early authentication failures remain uncorrelated and do not generate verbose warning logs.
Stale-authorization diagnostics cover checks after successful authentication, including closure races and fence failures.
Expired, removed, or otherwise unidentifiable registrations remain part of the documented authentication blind spot.
Do not reorder authentication, fencing, or parsing to improve observability.

### Request-local correlation and timing

Create a request-local diagnostic context immediately after successful authentication, before initial fence validation and JSON-RPC parsing.
Its ID is reused if the request becomes a tool invocation.
Malformed requests and other pre-dispatch rejections can carry `diagnostic_call_id` without a tool name.

Generate IDs independently of client-supplied values using a process-instance prefix and an atomic request sequence.
Establish the prefix once with a non-failing fallback if entropy acquisition is unavailable.
ID generation must not fail or block request admission.
Ensure distinct IDs for repeated and concurrent calls within the same Agent Turn.

Measure `duration_ms` from creation of this diagnostic context to each log event using a monotonic elapsed-time source where available.
Thus a later recording-failure entry may have a longer duration than its linked backend-failure entry.
Preserve existing ledger timestamp semantics independently of diagnostic timing.

Do not add correlation fields to durable invocation identity or mutation idempotency inputs.
Mutation-specific and detached execution logs remain outside this correlation guarantee.
Any later extension to detached mutations must explicitly carry diagnostic context into their separate operation context.

## 6. Improve existing read-failure logs

**Files:** `internal/mcp/failure.go`, `internal/mcp/gateway.go`, `internal/mcp/backend.go`

Carry safe diagnostics across the backend boundary and include them in `MCP read failed`.
Ensure credential failures, malformed backend results, scoped identity mismatches, cancellation, deadline expiry, and recording failures have explicit stages and safe reasons.

Use this event-count contract:

- A shared gateway rejection emits one `MCP request rejected` entry.
- A backend execution failure or malformed result emits one `MCP read failed` entry.
- A read-recording failure emits one additional `MCP read failed` entry with stage `read_recording`.
- Backend and recording failures have distinct diagnostics and the same `diagnostic_call_id`.
- Sending the final generic tool error does not produce a third duplicate rejection entry.
- Successful calls produce no failure entries.

Do not copy GitHub metadata from an execution failure onto a separate recording failure.
Keep ledger errors and agent-facing messages generic.

Example of intended output, not a diagnosis of the original incident:

```json
{
  "msg": "MCP read failed",
  "tool_name": "list_review_threads",
  "failure_code": "github_invalid_response",
  "failure_stage": "review_threads_validation",
  "failure_reason": "missing_field",
  "validation_field": "review_thread.originalLine",
  "github_http_status": 200,
  "diagnostic_call_id": "<gateway-generated-id>",
  "duration_ms": 42
}
```

## 7. Make ACP MCP failure summaries meaningful

**Files:** `internal/runtime/acp/agentevent.go`, `cmd/omnigrex/main.go`

Extend the existing fixed-message allowlist to recognize generic read/gateway messages, including:

- `tool call failed`
- `tool call could not be recorded`
- `tool is unavailable in this turn`
- Relevant fixed argument and authorization rejection messages when present in ACP output.

For a failed Omnigrex tool with no recognized classification, log `failure_class: "unclassified"` instead of an empty string.
Apply this fallback in the logging sink, leaving unknown Agent Event metadata unchanged.
Do not label successful updates as failures.

These remain summary logs.
They must not invent a GitHub cause, extract arbitrary runtime error text, or claim direct correlation between ACP tool-call IDs and gateway diagnostic IDs.
Apply narrowly scoped edits around existing ACP work rather than rewriting or replacing it.

## 8. Add regression and confidentiality tests

### GitHub tests

**Files:** `internal/github/api_test.go`, related error tests as appropriate

Use otherwise-valid HTTP fixtures with one failed rule per case to verify:

- Each instrumented validation rule produces the intended stage, reason, and field.
- Existing valid fixtures still pass and existing invalid fixtures still fail.
- Multi-defect fixtures preserve the first failing check and validation order.
- Pagination failures retain metadata from the response that exposed the failure.
- A failing continuation without a request ID does not inherit an earlier page's valid ID.
- A transport failure after a successful page carries no previous response metadata.
- HTTP `200` validation failures are not reported as HTTP rejections.
- Decode failures retain response metadata.
- GraphQL errors, no data, malformed envelope, and malformed data remain distinguishable.
- Cancellation before headers and during body reading retains safe classification.
- `errors.Is`, wrapping contracts, and retry-related behavior remain unchanged.
- Permission, rate-limit, transient, retry-after, and reset-time metadata survive redaction as before.
- Credential redaction preserves safe diagnostic structure.

### MCP tests

**Files:** `internal/mcp/gateway_test.go`, `internal/mcp/backend_test.go`, failure tests as appropriate

Verify:

- Precise GitHub diagnostics reach gateway logs.
- Credential-stage cancellation and deadline expiry are classified before cause information is discarded.
- Scoped issue identity mismatches have a precise diagnostic, preserve precondition matching, and omit unavailable response metadata.
- Early rejection branches produce one correlated log even before tool dispatch or envelope decoding.
- Authentication failures without trusted scope preserve existing quiet behavior.
- Rejected calls never reach the backend.
- Split checks preserve HTTP/JSON-RPC codes, messages, validation order, admission, and ledger behavior.
- Unknown status is omitted and malicious diagnostic values and request IDs are discarded.
- Repeated and concurrent calls in one Agent Turn receive distinct IDs.
- Backend and recording failures produce exactly two distinct, linked entries.
- The final generic tool response does not create a duplicate rejection entry.
- Duration follows the documented start point and per-event semantics.
- Agent-facing messages and ledger errors remain generic.
- Cancellation, deadlines, malformed results, and recording failures remain distinguishable.
- An independently returned validation failure is not mislabeled because the context later becomes canceled.
- Shared helper changes do not classify mutation validation failures after HTTP success as HTTP rejections or change durable mutation diagnostics.

### ACP summary tests

**Files:** `internal/runtime/acp/agentevent_test.go`, `cmd/omnigrex/main_test.go`

Verify:

- Known read messages receive safe classifications.
- Unknown failed MCP calls log `unclassified` without changing unknown Agent Event metadata.
- Successful updates are not labeled as failures.
- Raw runtime output does not enter logs.

### Confidentiality assertions

Inject sentinel secrets into credentials, response bodies, paths, arguments, remote errors, and unknown diagnostic fields.
Assert they never appear in logs or newly exposed error metadata.
Include invalid exported diagnostic values to verify boundary allowlists rather than relying only on constructors.
Prefer parsing JSON log entries and asserting fields and entry counts rather than depending on serialized field order.

## 9. Documentation and verification

Update the MCP troubleshooting section in `docs/operator-guide.md`:

- Explain the new fields, rejection logs, event counts, and duration semantics.
- Explain that HTTP `200` does not imply GraphQL or tool success.
- Document absent-status and rejected-request-ID semantics.
- Document metadata provenance and the backend scoped-identity mismatch exception.
- Show correlation using Agent Turn and `diagnostic_call_id`.
- Clarify that ACP tool summaries do not contain the authoritative backend cause or a guaranteed matching gateway call ID.
- Document runtime-local, authentication-scope, and mutation-specific coverage limits.
- Keep `diagnostic_call_id` and GitHub request IDs as correlation fields, not Loki stream labels or metric labels.

Run from the repository:

```sh
mise exec -- go test -race ./internal/github ./internal/mcp ./internal/runtime/acp ./cmd/omnigrex
mise run check
```

Run the narrowest relevant tests during implementation.
No deployment recreation or destructive Compose tests are required for this logging-only change.
If implementation unexpectedly changes Docker-backed behavior, follow the repository's integration-test requirements.

## Recommended implementation order

1. Define diagnostic allowlists, precedence, response metadata, and provenance rules.
2. Preserve safe cancellation classification at sanitization boundaries and protect shared mutation/retry behavior.
3. Instrument review-thread and issue-lookup validators plus backend scoped-identity checks.
4. Introduce request-local correlation and timing before pre-parse rejection points.
5. Add shared rejection logs and enhanced read logs with the explicit event-count contract.
6. Add ACP summary classifications and sink-only fallback.
7. Complete confidentiality/regression tests, documentation, and full verification.

## Acceptance criteria

- A repeat of the targeted read failures identifies the precise GitHub validation rule, a precise backend scoped-identity failure, or an earlier shared gateway rejection stage.
- Cancellation and deadline expiry remain distinguishable across the targeted sanitization boundaries.
- Response metadata refers only to the relevant response and never changes a successful HTTP response into rejection evidence.
- All covered logs from one request share a diagnostic ID, while separate requests receive distinct IDs.
- Execution and recording failures remain distinct without duplicate final-rejection entries.
- Existing acceptance rules, authorization, backend admission, responses, ledger behavior, mutation classification, and retry semantics are preserved.
- Credentials, arguments, GitHub content, and raw error messages do not enter logs or newly exposed metadata.
- Documentation accurately describes remaining coverage gaps.

This improvement cannot retroactively recover the missing cause from the original logs.
