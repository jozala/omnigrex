# Pi failure recovery and boundary independence

## Prerequisite and scope

This implements Step 13 of the [combined plan](2026-10-05-pi-and-pluggable-execution-backends.md), following [complete Pi Agent Turns](2026-10-09-complete-pi-agent-turns.md).
It applies the [execution and agent boundary contracts](2026-10-07-runtime-boundary-contracts.md) and the existing [settlement policy](../decisions/0016-qualify-pi-turns-with-existing-settlement-policy.md).

## Shared qualification boundary

`TestPiFailureRecovery` runs the same eleven scenarios through Docker and the native Microsandbox SDK.
Each scenario uses an isolated PostgreSQL database, real Git seed/remote, exact qualified fixture image, production Pi preparation and prompting, durable execution reservations, per-Turn authenticated MCP, trusted capture/publication, and the existing recovery workers.
The Step 12 complete Workflow fixture is shared with the successful Developer-to-Reviewer gate.
Its fixture-only controlled provider supplies fault modes; the production golden image excludes that provider.

Restart scenarios expire the complete durable authority chain, close the old Store/backend objects and transport, and construct fresh Store/backend objects against the same retained database, resources, and backend journal.
They do not adopt the old prompt or treat transport closure as termination.
Creation-response loss is injected after real environment creation succeeds, with the process handle withheld from the Launcher.
Cleanup uncertainty is injected at the neutral stop interface, leaving the actual environment available to the subsequent real recovery operation.
These deterministic boundary faults complement the existing backend dispatch-ambiguity, delayed-start, duplicate-inventory, and force-stop conformance tests.

## Observable fault matrix

| Scenario | Required observation |
| --- | --- |
| Restart during creation | Lost response retains recovery ownership; fresh inventory and the persisted reservation identify the exact unstarted environment. |
| Restart during prompting | Admitted prompt becomes unknown after detachment; the still-running environment is recovered by the fresh control plane. |
| Lost transport after admission | Detachment leaves positive running evidence and cannot supply a stop receipt. |
| Runtime exit 137 | The admitted prompt becomes unknown; exit code 137 alone never establishes OOM. |
| Resource exhaustion | Touched, retained allocations exceed the real environment memory budget and cause runtime exit; recovery uses only backend-owned cause evidence. |
| Lease expiry | The expired owner cannot validate its fence, reopen mutation admission, or dispatch stale execution after recovery. |
| MCP token expiry | An advanced gateway clock rejects the exact token with HTTP 401 while the database lease is otherwise live; no mutation is admitted. |
| Cleanup failure | Unconfirmed stop cannot seal terminal evidence; recovery retries without acknowledging stop or admitting a replacement. |
| Interrupted publication capture | A real quiesced tar transfer fails before sealing; no remote branch or Change Proposal is published, and later terminal capture succeeds. |
| Terminal mutation success with lost response | A signed native approval exists despite a lost HTTP response; recovery finds that exact review and reaches `PR_READY` with one submission. |
| Concurrent Turns | Each real Pi process reads its own Work Item through MCP; cleanup rejects the first token while the second process successfully reads again through its existing MCP connection, and retained histories stay isolated. |

Every restarted execution additionally experiences a failed stop-recovery attempt.
The durable recovery barrier denies completion and replacement admission until the real stop/capture/removal operation succeeds.
After success, a new Turn keeps the same Participant and Agent Session with a newer execution epoch; the old epoch cannot reserve execution again.
Repeated exact-key cleanup remains idempotent, and a removed resource supplies no fabricated exit or OOM evidence.

Publication interrupted before its proposed tip is recorded remains uncertain under the existing conservative mutation policy.
Read-only reconciliation exhausts its three attempts, creates exactly one Human Handoff, and does not repeat publication.
A lost native review response instead supplies an exact signed external artifact that read-only reconciliation and terminal corroboration can verify.
The shared assertions preserve this distinction rather than forcing every fault into a successful retry.

## Dependency enforcement

The normal repository gate checks transitive core import graphs for supported Darwin/arm64, Linux/amd64, and Linux/arm64 targets with default, integration, Microsandbox, and combined tags.
An additional production-source scan parses every internal Go file, including native CGO and platform/tag variants unavailable to the local compiler.
Only the composition package and concrete backend owners may import backend implementations or SDKs; external consumers of the Pi implementation are confined to composition.
Neutral contracts cannot import Store, Workflow, profile policy, or protocol implementations, and explicit host/container/volume/label fields and untyped exported option maps are rejected.
The existing ACP compatibility consumers remain scoped to their preparatory owners, as required by decision 0016; their removal belongs to Step 17.

## Verification evidence

The complete eleven-scenario matrix passed on Docker/Linux arm64 and Microsandbox/Linux amd64 with usable KVM.
The native gate used the explicitly approved Linux Docker host; connection values and deployment metadata are excluded here.
The tests use fixture-only credentials and controlled GitHub responses, with no live provider requests or external GitHub mutations.
Gate commands and applicable host prerequisites are authoritative in the [operator guide](../operator-guide.md#candidate-pi-agent-turn-qualification).

Observed passing commands:

```sh
mise exec -- go test ./internal/runtime/execution -count=1
mise run check
mise run test-integration
mise run test-pi-recovery
mise run test-pi-microsandbox-recovery
mise run test-pi-workflow
mise run test-pi-microsandbox-workflow
```

All eleven fault scenarios passed on each backend, and both complete Developer-to-Reviewer gates passed with the shared fixture refactoring.
The Docker integration suite passed, including Store recovery, publication, and settlement regressions; opt-in live and destructive Compose gates remained skipped by that suite's normal configuration.

The first independent review identified missing process-level concurrent MCP authority assertions and teardown through stale, closed Store/backend handles.
The concurrent gate now verifies actual scoped reads before and after neighboring cleanup, and fixture teardown performs exact-key removal through the current live backend before closing it.
The two maintainability findings were also addressed: execution handles and keys are paired in one fixture record, and dependency classification shares exact-package-or-subpackage matching across both checks.
Both final independent review axes reported zero remaining or new actionable findings.
After those corrections, `mise run check` and `mise run test-integration` passed again, and both backend Workflow entry points passed with `OMNIGREX_PI_WORKFLOW_TEST_PATTERN='^TestPi(FailureRecovery|ControlledWorkflow)$'`, covering the complete fault matrix and successful Workflow together.

## Delivery boundary

This step adds executable recovery qualification and dependency enforcement without introducing a new recovery policy or compatibility route.
Pi-to-Pi upgrade and compatible-host restore qualification remain Step 14.
