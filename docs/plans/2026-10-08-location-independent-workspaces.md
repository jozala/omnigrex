# Location-independent workspace evidence

## Scope and prerequisite

This records Step 05 of the [combined runtime plan](2026-10-05-pi-and-pluggable-execution-backends.md), following the [Docker execution boundary](2026-10-07-docker-execution-boundary.md).
OpenCode remains the production agent runtime.
Decision [0010](../decisions/0010-seal-portable-workspace-evidence-before-publication.md) fixes the initial capture encoding and retention policy under decision 0008.

## Mid-prompt mechanism proof

`TestPiMidPromptWorkspaceCapture` runs the pinned Pi 1.0.4 / Node 24.14.1 fixture with a background workspace writer while an authenticated MCP tool response remains outstanding.
Both backend experiments ran on an approved Linux/amd64 KVM Docker host, with Microsandbox 0.7.6 and its device group supplied ephemerally through the test environment.
No deployment address, credentials, or host configuration is recorded here.

- Docker pauses the exact Turn container and transfers workspace bytes through the Docker archive API.
- Microsandbox uses `msb pause --guest-flush required`, followed by trusted host-side transfer of its isolated mounted workspace and a bounded `msb resume`.
- Repeated reads while suspended are identical; writers advance before suspension and after resumption.
- Capture does not consume the held MCP response, settle the prompt, or require the agent to export its files.
- After resumption and explicit MCP response release, the same prompt settles normally and RPC remains usable.
- Retained transferred bytes remain independently inspectable after execution destruction.

The first Microsandbox attempt correctly failed because a plain host tar lost executable mode.
Microsandbox stores guest uid/gid/mode/type in the 20-byte version-1 `user.msb.override_stat` xattr and can represent guest symlinks as host regular files.
The successful mechanism proof preserves that xattr and decodes guest-visible modes and link targets with explicit version/type rejection.
The production Microsandbox adapter must repeat the proof and implement this translation in Step 08.

Run against an approved host using the connection and KVM environment setup in the [Pi experiment instructions](../../experiments/pi/README.md):

```sh
OMNIGREX_PI_FEASIBILITY=1 OMNIGREX_PI_BACKEND=docker \
  go test -race -tags=integration -timeout 25m -count=1 -v ./experiments/pi -run '^TestPiMidPromptWorkspaceCapture$'
OMNIGREX_PI_FEASIBILITY=1 OMNIGREX_PI_BACKEND=microsandbox \
  go test -race -tags=integration -timeout 25m -count=1 -v ./experiments/pi -run '^TestPiMidPromptWorkspaceCapture$'
```

## Production boundary

Preparation and discard use `execution.WorkspaceRef`, which binds the checkout to its exact Participant, Session, Turn, epoch, and seed revision without exposing a host directory to the Launcher.
Trusted local acquisition still uses the existing fenced checkout promotion internally.
Repository tool-storage paths remain transitional until Steps 06-07.

`execution.WorkspaceSource` supplies a complete guest-semantic tar while holding the writers quiescent for the entire sealing callback.
The Docker source revalidates immutable container identity, pauses live publication writers, and resumes under a fresh bounded cleanup context even when transfer fails or is cancelled.
Terminal requests require positive stopped-environment evidence.
No direct-live-publication fallback remains.

The workspace service normalizes the archive, verifies completeness and limits, seals its content and versioned manifest atomically, and verifies retained bytes when reopening.
Migration 000031 independently records immutable capture identity and digest in PostgreSQL, so losing an entire artifact directory cannot authorize a newer observation under the same operation.
Manifest/content sealing finishes before the ledger fact is acknowledged; retrying an interrupted acknowledgement adopts only the already sealed bytes.
Standalone qualification fixtures use an independent local seal inventory, while production always supplies the Store ledger.
Original Git objects, HEAD/index, tracked files, untracked inventory, modes, and symlinks survive transfer.
Unsupported entries, traversal, duplicate entries, symlink ancestors, missing required Git inventory, truncated archives, and nonzero data after the archive terminator are rejected.
Inspection strips captured executable configuration, hooks, worktree overrides, and history indirection before using trusted Git.
Existing first-parent, default-branch merge-parent, multi-commit identity, tree comparison, exact remote-head, and uncertain-push reconciliation remain authoritative.

## Finalization and recovery

Finalization closes mutation admission, drains MCP, confirms stop, and seals a fresh terminal capture before environment removal or Reviewer workspace discard.
Transfer runs outside the short database fences; admission and adoption are checked separately so heartbeats and single-connection pools remain usable.
Capture failure retains the resources needed to retry.
Preparation failures with durable current-execution proof that prompting was impossible can clean up without terminal capture.
Recovery revokes dispatch and distinguishes unlaunched executions and authoritative no-effect creation from ambiguous main dispatch or recorded Turn activity.
A historical Session binding alone is not current-Turn prompt admission.
For first-Session startup failures, the Launcher must commit a fenced, monotonic preparation-only cleanup proof before deleting a bound execution without capture.
The proof requires an unbound Creating Session and no prompt or tool activity; failed proof admission preserves the environment for recovery.
Recovery can also record a preparation-only proof from positive backend no-start evidence for the exact main resource, including a retained Session's new execution.
Docker supplies this evidence only for `created` with an explicit valid zero `StartedAt`, no PID, and no running, paused, restarting, dead, or OOM state; missing timestamps remain unknown and no exit is fabricated.
The recovery fence must already deny new prompts and creation, the revoked reservation must match that immutable resource, and durable prompt/tool activity must be absent.
After recording the monotonic proof, cleanup removes the exact ID before acknowledging stop, so delayed native starts cannot survive successful recovery.
The archive source continues rejecting `created` as terminal-capture evidence; observing that state alone does not establish a consistent final snapshot while an old start may still be pending.

Recovered cleanup first revokes further creation and confirms exact-key workloads stopped without deleting their workspace.
It then seals or reopens the original epoch's terminal evidence before removal and durable stop acknowledgement.
Missing execution and missing capture are unavailable evidence, never fabricated success.
Outcome reconciliation and deferred corroboration consume only the original Turn's capture and privately materialized publication checkout.
A fresh trusted clone is fetched at the exact terminal intent's published SHA, with orchestrator credentials, so deleting or replacing publication caches does not affect corroboration or restore.
A later checkout cannot replace that evidence.

Captures live under `workspace-captures` in the retained-state volume, outside the Session-state subpaths mounted into agents.
They remain available through deferred verification, human revalidation, and restart, and are deleted only under Participant collection authorization.
Operational backup and restore procedures remain in the [operator guide](../operator-guide.md#backup).

## Verification coverage

The tests include real Docker archive transfer, writer races, resume after failed transfer, full streamed seed materialization, stopped capture before removal, and retained inspection after restart without access to the live checkout.
Git regressions exercise exact multi-commit/merge history, dirty tracked/staged/untracked data, invalid history and metadata, executable files, symlinks, remote-head races, uncertain publication, late edits, missing/corrupt evidence, and operation-idempotent retries.
Store tests exercise original-Turn deferred corroboration, cleanup recovery, durable preparation-only exemptions, capture transfer outside a single-connection fence, and retained detection of unpublished work after workspace replacement.
`TestNeverStartedRuntimeRecoveryCompletesWithRealDocker` exercises real Docker, PostgreSQL, and the StopWorker through removal and durable acknowledgement after interrupted creation/startup, rejected native start, lost creation acknowledgement, and execution in an already-bound Session.

Final verification passed:

```sh
mise install
mise run check
mise run test-integration
```

The final Docker-backed integration gate passed doctor, ACP, Docker, Store, and deployment suites with the race detector.
The final actual-Pi mid-prompt mechanism tests passed on Docker and Microsandbox in approximately 27 seconds and 37 seconds respectively.
Independent Standards and Spec reviews found no remaining actionable findings after their recovery and immutable-evidence corrections were applied.

The destructive Compose gate is excluded from this verification because it recreates stable local networks and volumes.
