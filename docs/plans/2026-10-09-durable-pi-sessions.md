# Durable Pi Agent Sessions and History Replay

## Prerequisite and scope

This implements Step 11 of the [combined plan](2026-10-05-pi-and-pluggable-execution-backends.md), following [Step 10 configuration and MCP](2026-10-09-pi-configuration-and-mcp.md).
It follows the [runtime boundary contracts](2026-10-07-runtime-boundary-contracts.md#6-agent-and-mcp-contracts), [decision 0015](../decisions/0015-reserve-exact-pi-sessions-and-replay-bounded-history.md), and [ADR 0003](../adr/0003-orchestrator-owns-session-control.md).

## Durable ownership and preparation

Migration 34 adds separate runtime Session identity and capability fields without altering existing ACP conversation bindings.
The Store reserves the durable Agent Session UUID as the exact Pi identity before any runtime bootstrap and activates only that reserved identity under the still-live Agent Turn fence.
Identity and capability bindings are immutable, and the active-Session constraint accepts independently validated ACP or Pi bindings.
New Pi Participants receive the `pi-session/v3` retained-state format; existing Participant storage bindings remain untouched.
Current Pi Profile snapshots support thinking and reject OpenCode-only fields at the Store boundary as well as the Profile parser.

`internal/runtime/session.RuntimeCoordinator` depends on neutral agent operations and the narrow Store admission interface.
Preparation checks logical state ownership, reserves identity, installs exact per-Turn inputs through the composition callback, verifies readiness, binds capabilities, and records protected-state initialization.
It never submits a prompt.
Prompting requires a finite caller deadline, exact durable identities, the same prepared runtime connection, and a live prompt fence.
Existing adapter cancellation bounds runtime work after a deadline, while environment stop/removal remains owned by the execution layer.
The owned runner keeps provider retries disabled; the separate protocol qualification continues exercising bounded retry and retry-backoff cancellation.

## Exact Session persistence

The runner opens `/state/sessions/<durable-session-uuid>/session.jsonl` inside the admitted Participant state attachment.
These are guest paths and do not imply shared orchestrator filesystem access.
Creation publishes a private synced header atomically before readiness, so interrupted preparation can reopen the same identity even without a first model prompt.
Activated Sessions require the exact existing file and cannot choose a latest conversation or create a new one after protected-state loss.
The owner validates version, identity, stable guest workspace, complete UTF-8 JSONL records, entry identities and parent chains, required message/content shapes, and context-affecting references before invoking Pi's parser.
Compaction retained-entry and context-edit targets must belong to the applicable ancestor branch; invalid references cannot silently erase or replace conversational context.
Symlink aliases are rejected.
It never asks Pi to migrate an old file or repair malformed input.
Fresh Runtime Processes receive disposable auth and authenticated MCP inputs independently of the retained Session and replacement workspace.

## History Replay and human control

Replay observes complete append-order history, not Pi's compacted or current-branch model context.
The owned input multiplexer handles one read-only history operation and forwards ordinary commands to Pi's existing RPC reader.
Its snapshot pages contain at most 128 metadata entries and remain below the existing 1 MiB record ceiling even when the transcript is larger.
The Go adapter translates messages, provider outcomes, tool results, model/thinking changes, compactions, branch summaries, context edits, and other known entries into operational Agent Events.
It does not emit message content, tool arguments/results, historical instructions, summaries, or source errors.
Events retain durable Participant/Session correlation, original entry timestamps, and entry/parent identity.

A serializable cursor is scoped to one exact runtime Session and advances only through acknowledged events.
Replay excludes prompt admission, rejects foreign or malformed cursors and inconsistent snapshots, and bounds stalled sinks.
Human prompting acquires the existing exclusive durable admission lease before exact reopening, verifies the retained capability contract, completes full replay, renews the fence, and then submits one prompt.
Replay failure prevents submission.
An unknown admitted outcome keeps its token rather than permitting overlapping replacement work; confirmed settlement permits exact-token release.
This is the internal human prompt/replay capability, not a new human UI or execution-launch workflow.

## Acceptance coverage

The shared actual-Pi suite runs through Docker execution and the native Microsandbox Go SDK.
Developer and Reviewer scenarios run concurrently with independent state attachments, disposable credentials, and authenticated fixture authority.
Each creates state before a first prompt, discards the first preparation result, removes the entire environment, recovers the exact Session, submits a prompt, and then reopens it in another environment with a fresh workspace.
Replay covers more than 1 MiB of persisted history and over two pages, including an abandoned branch, a branch summary, and compaction.
The provider verifies prior context is retained, abandoned history is absent from current context, and replay made zero provider requests in the new environment.
The suite audits retained files for fixture credentials and rejects missing activated state before admission.

Public-boundary tests cover append-order cursors, sink acknowledgement failure, bounded stalled consumers, prompt exclusion, corrupt history, stale Store fences, wrong identities, wrong live connections, immutable capabilities, replay-before-human-prompt, and uncertain human writer barriers.
Owned file tests cover header persistence, exact recovery, missing activated state, wrong headers, duplicate entries, truncation, old formats, symlinks, malformed messages, and missing or wrong-branch context references without modifying rejected input.
The standalone `pi-diagnostic --sessions` exercises reachable candidate coordination with a disposable in-memory ledger; PostgreSQL tests supply the durable Store evidence.
Operational commands are authoritative in the [operator guide](../operator-guide.md#pi-protocol-diagnostic).

## Verification evidence

Observed passing commands include:

```sh
mise install
mise run check
mise exec -- go test -race ./internal/runtime/session ./internal/runtime/pi
mise exec -- go test -race -tags=integration ./internal/store -run 'TestDurableRuntimeSessionReservationAndActivationAreFenced|TestHumanPromptCoordinator|TestHumanPromptAdmissionLease' -count=1
mise run test-integration
mise run test-pi-integration
mise run test-pi-microsandbox-integration
git diff --check
```

The Linux/amd64 KVM gate used the explicitly approved remote Docker connection and its numeric KVM group.
Connection values and deployment metadata are excluded from this record.
Both complete Pi gates passed after paginated replay and strict context validation, covering twenty configuration scenarios, parallel Developer/Reviewer continuation across four fresh environments each, and four standalone diagnostic variants.
The existing Docker integration gate initially exposed six stale assertions expecting schema version/count 33; updated expectations require migration 34 while preserving the original preservation and concurrency checks, and the complete gate then passed.
An early external command timeout interrupted the first remote image transfer before a qualification wrapper remained running; the repeated gate completed with an appropriate command budget.
Destructive stable-stack Compose recreation and live provider/GitHub qualification were not part of this preparatory milestone.

### Independent review follow-up

The first Standards review reported zero hard breaches and two non-blocking maintainability smells: duplicated human-admission heartbeat code and an ACP-named argument carrying both runtime identities.
The heartbeat now has one shared protocol-independent helper, and the admission argument uses runtime-neutral terminology.
The independent Spec/correctness review reproduced malformed user content and an invalid compaction retained-entry reference passing exact reopening and silently changing restored context.
A public reopening regression failed before the correction.
The owner now validates all supported entry payloads and branch-sensitive references before Pi can normalize them, including missing content, unknown and wrong-branch compaction targets, missing context-edit targets, and malformed tool results.
Both complete Pi backend gates passed after these corrections.
The final Standards reassessment confirmed both maintainability findings resolved and reported zero hard breaches, actionable smells, or unresolved findings across all thirty-seven files.
The final Spec/correctness reassessment approved Step 11 with zero remaining findings, independently reproducing rejection and byte preservation for both original defects and invalid UTF-8 against the pinned Pi SDK.
It also verified successful reopening of legitimate SDK-generated messages, usage, assistant/tool-result context edits, labels, custom entries, and compaction.
After both clean reassessments, the normal check and the complete existing Docker integration gate passed again against the final code.

## Compatibility and next step

The existing OpenCode deployment and its ACP-specific coordinator remain usable until their assigned removal in Step 17.
The new runtime coordinator and owned Pi persistence path are reachable through candidate diagnostics and qualification.
Step 12 connects them to complete Agent Turns on both backends, including production MCP registration and outcome settlement.
Pi upgrade/restore qualification, published golden images, operational cutover, and the full restart fault matrix remain assigned to later steps.
