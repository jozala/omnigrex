# Pi protocol adapter

## Scope and prerequisite

This implements Step 09 of the [combined implementation plan](2026-10-05-pi-and-pluggable-execution-backends.md), following the [Microsandbox backend](2026-10-09-microsandbox-backend.md).
It follows [decision 0006](../decisions/0006-use-pi-through-an-agent-specific-adapter.md), the [agent boundary contracts](2026-10-07-runtime-boundary-contracts.md#6-agent-and-mcp-contracts), and the observed [Pi 1.0.4 protocol](../experiments/pi.md).

`internal/runtime/agent` exposes runtime-neutral preparation, prompt outcomes, cancellation, and transport detachment.
`internal/runtime/pi` implements those operations over a backend-supplied non-TTY transport.
The diagnostic command makes the implementation reachable without changing the default OpenCode Agent Turn path.
Configuration, authenticated MCP integration, instructions, durable Session ownership, and History Replay are delivered in Steps 10-12.

## Protocol behavior

The adapter continuously reads LF-delimited JSON records and correlates responses by generated request IDs and expected commands.
Responses can arrive out of order, and events can arrive before the prompting caller starts waiting.
Command responses, event delivery, and writes have separate bounded paths.
The implementation limits individual records to 1 MiB, outstanding commands to 32, and queued operational events to 128.
Overflow fails explicitly instead of accumulating an unbounded transcript or blocking the protocol reader behind a slow sink.

Preparation verifies an idle runtime and, when supplied, the exact expected runtime session identity.
This is a protocol readiness check rather than the durable fenced Session preparation required by Step 11.
A successful `prompt` response with `disposition=started` establishes admission and emits a neutral admission observation.
Intermediate `agent_end`, retry events, and assistant failures do not settle the Agent Turn.
Only `agent_settled` together with the final assistant stop reason and verified retry-cancellation evidence establishes completed, failed, or aborted outcomes.
A terminal record delivered immediately before EOF remains valid.
Missing terminal evidence, malformed responses, transport loss, and deadlines return unknown outcomes rather than fabricated completion or environment-stop evidence.

Cancellation requests Pi abort and waits for settlement under a finite grace period.
Cancellation holds the prompt-admission barrier until its correlated response and settlement are observed.
An uncertain cancellation or interrupted prompt poisons the connection, preventing a later prompt from overlapping uncertain work.
Closing the connection is bounded transport detachment; the execution owner remains responsible for whole-environment termination and removal.

Agent Events retain their supplied domain correlation fields and contain bounded operational metadata.
Pi-specific diagnostics are reconstructed from allowlisted event names and assistant stop reasons.
Retry-backoff cancellation additionally records a normalized boolean while preserving the failed attempt's actual assistant stop reason.
Source provider errors, assistant content, tool arguments/results, raw JSON, and stderr are never retained in public errors or emitted diagnostic events.
Provider failures therefore remain distinguishable without copying secret-bearing source objects.

## Disposable diagnostic path

`omnigrex pi-diagnostic` starts the selected backend's disposable protocol probe, prepares the connection, submits one controlled prompt, and requires settled success plus cleanup.
Both backends provide separate stderr and expiring diagnostic ownership without Participant attachments or retained Session resources.
Protocol probes carry an installation namespace and unique probe identity, and recovery revalidates installation ownership before cleanup.
Docker uses the deployment's runtime-state volume name as its installation diagnostic namespace; Microsandbox uses its configured inventory namespace.
Legacy ACP diagnostics retain their transitional cleanup path until Step 17.
The probe image pins Pi 1.0.4, Node 24.14.1, the base image digest, and the existing npm dependency lock.
Its controlled HTTP provider uses only a nonsecret fixture credential and persists configuration and Session files in disposable tmpfs.
Docker diagnostics accept content-addressed local image IDs as well as exact registry digests; this does not change protected Participant image admission.
Microsandbox qualification imports a disposable archive and validates its exact manifest digest through the existing native image boundary.
Operational reproduction commands are authoritative in the [operator guide](../operator-guide.md#pi-protocol-diagnostic).

## Acceptance coverage and observed evidence

The same production adapter suite runs through Docker attach and Microsandbox Go SDK streams.
It exercises actual pinned Pi RPC and a controlled HTTP provider, including successful streaming with Unicode separators, one transient retry, provider failure after admission, lost settlement, held-request timeout, explicit cancellation, primary-process exit, and malformed output.
The process-loss outcome remains unknown even when the fixture process exits with a known nonzero code.
The subprocess diagnostic command is also exercised on both backends.

Public-boundary regressions cover admission versus settlement, out-of-order responses, terminal delivery immediately before EOF, concurrent prompt rejection, cancellation, slow consumers, event overflow, blocked writes, bounded transport closure, malformed correlation, and credential-safe failures.
The dependency gate prevents core coordination and neutral contracts from reaching the Pi implementation or either execution backend under all supported build-tag combinations.

During qualification, the diagnostic proxy's use of Node readline split valid JSON strings at Unicode separators and crashed its process.
The corrected fixture frames exclusively on LF, matching the Go adapter and the previously qualified Pi protocol.

The first independent reviews identified delayed-write acknowledgement losing buffered settlement, malformed acknowledgements claiming admission, and missing Docker diagnostic installation ownership.
Public-boundary regressions now reproduce those cases and verify authoritative correlated responses, unconfirmed admission after malformed responses, and installation-scoped inventory/cleanup.
The native diagnostic subprocess test passes its fixture's explicit cache and runtime paths instead of relying on duplicated defaults.
The second correctness review reproduced a queue-empty/settlement race that dropped the terminal Agent Event, and cancellation choosing EOF over already-confirmed abort settlement.
The prompt now returns its terminal outcome only after delivering the queued terminal event, while cancellation preserves positive settlement evidence across EOF.
Repeated public-boundary stress regressions cover both corrected interleavings.

Observed passing commands include:

```sh
mise install
mise run check
mise run test-integration
mise run test-microsandbox-integration
mise run test-pi-integration
mise run test-pi-microsandbox-integration
mise exec -- go test -race ./internal/runtime/pi ./internal/runtime/docker ./internal/runtime/microsandbox ./internal/runtime/execution ./cmd/omnigrex
mise exec -- go test -race ./internal/runtime/pi -run 'TestSettledOutcomeAlwaysDeliversTerminalEvent|TestConfirmedAbortSettlementSurvivesEOF' -count=6
git diff --check
```

The Linux/amd64 KVM gates used the operator-approved Docker connection and numeric KVM group, verifying the pinned Microsandbox 0.7.6 SDK path without exposing the host Docker socket to the guest.
The full existing Docker and KVM backend suites passed after the diagnostic ownership corrections.
After the final protocol corrections, the normal local check and both complete Pi backend gates passed again, including their diagnostic subprocess tests and permanent race regressions.
The 20,000-prompt stress test has a separate one-minute workload budget for race-detector overhead on the two-vCPU qualification wrapper; individual deadline and bounded-cleanup assertions retain their focused finite budgets.
The implementation completed three independent Standards and Spec/correctness review rounds.
The final Standards review reported zero actionable violations or smell findings, and the final Spec/correctness review approved Step 09 with no remaining actionable findings.
The correctness reviewer independently ran six race-enabled Pi suite repetitions, covering 120,000 terminal-event checks and 600 abort/EOF attempts, plus the original outside-repository reproductions.
Destructive stable-stack Compose recreation and live provider/GitHub gates were not run for this protocol milestone.

### Retry-backoff cancellation follow-up

Review of `9ba8e96` identified that Pi 1.0.4 can abort its retry backoff without appending another assistant message, leaving the preceding attempt's `error` stop reason in place.
The shared controlled-provider regression first reproduced that incorrect provider-failure classification on the production adapter.
It configures a 30-second Pi retry delay, waits for `auto_retry_start`, and requests cancellation before another attempt can begin.
The adapter now recognizes Pi's exact failed-retry cancellation marker only when Omnigrex requested an abort during that failed attempt's backoff, and reports `Aborted` only after `agent_settled`.
The terminal diagnostic retains the original assistant `error` reason and a safe `retry_cancelled` boolean rather than copying `finalError` text.
Public-boundary regressions preserve provider-failure classification when cancellation evidence is absent or merely resembles the marker, and preserve unknown outcomes when settlement is missing.
The corrected controlled-provider suite passed on Docker and the native Linux/KVM Microsandbox backend, including the held-backoff regression and both diagnostic subprocess checks.
`mise run check` and ten race-enabled repetitions of the focused marker/settlement regressions passed for the follow-up.
Independent Standards and Spec/correctness reviews approved the correction with no actionable findings; the correctness reviewer additionally reran the actual Docker held-backoff regression three times.

## Temporary compatibility and next step

The existing OpenCode adapter, Agent Turn wiring, and default Runtime Profile remain operational until their assigned cutover steps.
The disposable Pi fixture is a protocol diagnostic rather than a production repository golden image or a credential mediation service.
Steps 10-12 extend the adapter with checked configuration, the owned MCP extension, current instructions, authorized repository skills, durable Session Continuation, History Replay, and full Agent Turn composition.
No Workflow package consumes Pi RPC structures.
