# Separate Pi startup readiness from ordinary RPC deadlines

## Delivery context

This corrective work is planned between Steps 16 and 17 of [Pi runtime and pluggable execution backends](2026-10-05-pi-and-pluggable-execution-backends.md).
Step 16's source implementation and isolated rehearsals are complete, while its published-release gates and Step 17's Pi-only qualification and production cutover remain outstanding.
An operator's live candidate deployment exposed the startup failure described below.
This document proposes a fix; it does not claim implementation or qualification.

Relevant delivery records are [Step 15 operational integration](2026-10-11-pi-operational-integration.md) and [Step 16 operator documentation and cutover rehearsal](2026-10-11-pi-operator-documentation-and-cutover.md).
The [operator guides](../operators/README.md) remain authoritative for deployment and recovery procedures.

## Problem and observed evidence

A Linux/amd64 Microsandbox candidate deployment passed runtime diagnostics but failed Agent Turn initialization after repository mise configuration was added to supply Go.
The earlier Agent Turn had reported that Go and gofmt were unavailable because the repository did not declare its toolchain.
Adding a repository toolchain declaration was correct, but exposed a deadline coupling between repository bootstrap and Pi protocol initialization.

The orchestrator reported `launch Agent Turn Runtime Process: prepare Agent Session: context deadline exceeded` and settled the affected Turns as `INFRASTRUCTURE_FAILED` with status `TIMED_OUT`.
The configured overall Agent Turn budget was two hours, with 1 GiB of guest memory.
Microsandbox logs showed successful guest readiness followed by disconnection of the active execution session approximately thirty seconds later.
Repeated guest process snapshots showed `omnigrex-environment` with a child `mise` process and no Node process through approximately thirty seconds of guest uptime.
The environment was subsequently removed, and retries repeated initialization.

These observations strongly support startup interruption while repository bootstrap was still running, before Pi could respond.
They do not establish whether mise was downloading Go, resolving activation, or waiting on another operation.
They do not establish an OOM cause or a guest crash preceding the deadline.
The current error does not distinguish initial configuration delivery from the first readiness RPC wait.

## Current implementation

- `cmd/omnigrex-environment/main.go` calls repository bootstrap before executing Node and the owned Pi runner.
- `internal/runtime/environment/bootstrap.go` runs `mise install --yes` and `mise env --json` when supported repository configuration exists.
- Repository installation output is discarded, and installation data is reset each Turn, so retries cannot be assumed to warm the installation cache.
- `internal/runtime/pi/connection.go` defaults `Options.RequestTimeout` to thirty seconds and applies it to RPC exchanges.
- `internal/runtime/pi/configuration.go` also uses that budget for delivery of the initial configuration frame.
- `internal/runtime/pi/turn.go` leaves `RequestTimeout` unset in production.
- `CallerDeadlineOnly` controls prompt timeout behavior, not the request timeout.

The process transport becomes available before repository bootstrap completes.
The control plane starts protocol initialization over that transport even though Pi is not yet available to read and answer requests.
Consequently, an ordinary RPC deadline also limits potentially lengthy repository tool setup.

## Proposed design

### Distinct budgets

| Budget | Proposed default | Scope |
| --- | --- | --- |
| Pi startup | 10 minutes | Initial configuration delivery, repository bootstrap, Pi initialization, and initial readiness verification |
| Ordinary RPC request | Existing 30 seconds | Individual requests after the initial readiness barrier |
| Overall Agent Turn | Existing 2 hours | All preparation and execution, including startup |

Expose the startup budget as `OMNIGREX_PI_STARTUP_TIMEOUT`.
Validate it as a positive, bounded duration using existing configuration conventions.
The effective deadline is always the earliest applicable deadline, including the caller's existing deadline.
The startup budget is one absolute deadline beginning when the adapter attaches to the started Runtime Process, before initial configuration delivery.
Do not restart that budget for each initialization step.
Cancellation and cleanup retain their existing independent budgets.

### Readiness sequence

1. Start the Execution Environment and repository bootstrap through the existing launch path.
2. Establish the absolute adapter startup deadline.
3. Deliver the bounded initial configuration frame under that deadline.
4. Send the initial `get_state` request and await its response under the same deadline, without imposing the ordinary thirty-second request limit.
5. Validate the response and exact Session identity.
6. Use ordinary request deadlines for model/thinking, permissions, and MCP verification, while retaining the remaining absolute startup deadline as an outer bound.
7. Complete the existing durable Session binding and fence checks.
8. Permit prompt admission only after complete preparation succeeds.

A successful initial `get_state` response establishes that Pi's RPC loop is responding.
It does not independently authorize prompt admission or substitute for the remaining configuration and authority checks.
Subsequent preparation on an already initialized connection must use ordinary request budgets rather than reopening a long startup window.

### Preserve the existing guest protocol

Use the first successful `get_state` response as the readiness barrier instead of requiring a new bootstrap-ready frame.
Retained Participants execute their exact older images, which would not emit a newly required frame.
The existing response works with those images and requires no Docker- or Microsandbox-specific readiness logic.

Do not treat `omnigrex_session_ready` as RPC readiness.
The owned runner emits it after opening the Session file but before completing runtime initialization.
Its existing Session identity and capability validation remains necessary.

The guest runner's separate thirty-second initial-configuration read timeout begins after Node starts.
It is not the deadline interrupting mise in the observed failure and remains unchanged.

## Implementation scope

### Pi adapter

Add `StartupTimeout` and explicit initial-startup state to the Pi connection options and implementation.
Store or otherwise carry one absolute startup deadline across initial configuration delivery and preparation.
Refactor request handling to accept an explicit startup or ordinary-request deadline policy while sharing correlation, bounded queues, transport failure handling, and response/EOF reconciliation.
Do not duplicate the request implementation or globally increase `RequestTimeout`.

Apply the startup deadline to the initial write in `internal/runtime/pi/configuration.go`.
A blocked write remains cancellable and bounded.
On failed initialization, close the connection so an outstanding write cannot continue independently of failed preparation.

Pass the configured startup budget through `internal/runtime/pi/turn.go` and production composition.
Preserve shorter caller budgets for doctor and disposable diagnostics.

### Configuration and documentation

Wire the setting through `internal/config`, production composition in `cmd/omnigrex/main.go`, Pi Compose environment forwarding, and the deployment environment example.
Update the operator guide to distinguish startup, ordinary RPC, and overall Agent Turn deadlines.
Review standalone and backend-selected diagnostic composition so the new default cannot extend existing outer diagnostic deadlines.

### Operational visibility

Add INFO-level start and completion observations for the startup-readiness wait.
Include Agent Turn and Session identifiers, the startup budget, duration, and safe outcome classification.
Distinguish timeout, transport failure, and configuration failure where supported by evidence.
The observed phase includes repository bootstrap and runner initialization; do not label it a Go download failure without evidence.

Keep arbitrary repository installation output, provider configuration, and MCP bearer authority out of ordinary logs.
Detailed bootstrap-progress instrumentation is a separate improvement.

## Failure and recovery invariants

On startup timeout, report a phase-specific failure such as `Pi startup readiness deadline exceeded`.
Do not submit a model prompt or adopt an incompletely prepared Session.
Use existing fenced cleanup, stop evidence, and settlement paths.
A timeout or closed transport does not prove that execution has stopped.
Do not admit a replacement until existing execution exclusion requirements are satisfied.

Do not change Workflow transitions, retry policy, retained-state ownership, tool-cache behavior, or model configuration as part of this fix.
Do not introduce execution-backend branches into core coordination.

## Compatibility and alternatives

Increasing the overall Agent Turn budget does not address the separate request deadline.
Globally increasing the ordinary RPC timeout weakens detection of an unresponsive ready runtime and retains the startup/protocol coupling.
A new required guest bootstrap marker would require explicit resource-contract and retained-image compatibility handling, which this fix avoids by using existing RPC readiness.
Preinstalled repository tools can reduce startup latency but do not repair deadline semantics for other repository installation scripts.

The proposed fix changes control-plane adapter behavior and deployment configuration.
It should not require modifying the Pi golden image, Session format, immutable Runtime Profile contract, or retained Participant bindings.
Preserve the policies in [decision 0014](../decisions/0014-own-pi-resource-loading-with-a-pinned-runner.md) and [decision 0016](../decisions/0016-qualify-pi-turns-with-existing-settlement-policy.md).
During implementation, record the consequential startup-budget and readiness policy as a proposed decision under the project's decision-record guidance rather than treating this plan as an accepted decision.

## Verification

Use short configurable budgets in focused tests instead of repeated thirty-second sleeps.

| Scenario | Required result |
| --- | --- |
| Initial configuration reader delayed beyond ordinary request budget | Succeeds within startup budget |
| First `get_state` response delayed beyond ordinary request budget | Succeeds within startup budget |
| Configuration delivery and readiness together exceed startup budget | Times out against one shared deadline |
| Post-readiness RPC stalls | Ordinary request deadline applies |
| Parent deadline is shorter | Parent deadline wins |
| Cancellation during startup wait | Connection closes and existing cleanup executes |
| Wrong Session identity or malformed response | Preparation fails |
| Startup failure | No model prompt is admitted |
| Repeated preparation of an initialized connection | No renewed long startup allowance |
| Doctor or diagnostic has a shorter outer budget | New startup default does not extend it |

Add a shared Docker/Microsandbox integration scenario with deliberately delayed repository bootstrap exceeding the ordinary request budget.
Verify initialization succeeds and a controlled prompt completes afterward.
Also verify startup timeout cleanup and the existing barrier against overlapping replacement execution.
Use controlled fixtures, not real provider calls or live GitHub mutations.

Run the narrow regressions, `mise run check`, applicable Docker integration tests, and relevant Pi qualification suites on both backends.
Run Linux/KVM verification for the native path using the documented backend gates.

## Rollout and acceptance

Rebuild and publish the selected control-plane candidate, retaining the existing exact Pi image and Participant bindings.
Apply the configured startup budget through the deployment's complete selected Compose file set and rerun doctor.
Retry a controlled Work Item whose working checkout declares Go, and confirm tool setup completes and Pi reaches readiness within the startup budget.
Successful doctor alone is not evidence that repository tool installation or the live Workflow succeeds.

Acceptance requires a bootstrap lasting longer than the ordinary thirty-second RPC budget to succeed within its startup budget, while an unresponsive already-ready Pi remains bounded by the ordinary request deadline.
Record verification evidence before continuing final qualification toward Step 17.
