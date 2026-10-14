# Microsandbox execution backend

## Scope and prerequisite

This implements Step 08 of the [combined implementation plan](2026-10-05-pi-and-pluggable-execution-backends.md), following the [repository environment contract](2026-10-08-repository-environments.md).
The implementation follows the [neutral boundary contracts](2026-10-07-runtime-boundary-contracts.md) and [decision 0013](../decisions/0013-use-the-microsandbox-go-sdk-and-posix-bindings.md).
OpenCode remains the transitional runtime while the subsequent steps implement Pi.

## Adapter and composition

`internal/runtime/microsandbox` implements the execution factory, process transport, paginated inventory, exact-key recovery, consistent workspace source, image availability, and disposable diagnostics.
Its private SDK bridge uses Go SDK 0.7.6 and an installed Microsandbox 0.7.6/runtime-firmware pair.
SDK objects and event framing do not reach Workflow, Agent Turn, Store, or neutral public signatures.
Composition selects one backend for launch, cleanup, startup recovery, captures, retained-state access, readiness, doctor, and offline Session recovery.
No backend branch is added to Workflow or Agent Turn policy.

The native implementation is built with CGO and the `microsandbox` build tag.
`Dockerfile.microsandbox` supplies the pinned native runtime and a separate qualification target.
The default static Docker image remains available and explicitly rejects Microsandbox selection.
Host and configuration procedures are authoritative in the [operator guide](../operator-guide.md#microsandbox-execution-backend).

## Identity, provisioning, and recovery

Store reservations commit the exact key and nonsecret specification digest before main environment creation.
A private, fsynced, backend-owned operation journal serializes dispatch, capture, stop, and removal across control-plane processes with file locks.
The journal records the immutable SDK resource identity, preparation/start barriers, revocation, required-flush evidence, and observed primary-process exits.
It contains no agent environment or credential bytes and is outside every guest mount.
Changing the request, reusing a started epoch, replacing an SDK identity, or dispatching after revocation fails explicitly.

Long image checks, isolated golden-resource qualification, VM boot, credential transfer, cgroup setup, and private Docker startup run during environment creation outside the Agent Turn row lock.
Start performs only the bounded primary-process attachment under the existing fence.
Golden-resource diagnostics have separate expiring identities and no Participant attachments, provider credentials, or business MCP authority.

An ambiguous creation remains unknown until its exact resource can be resolved.
Empty or partial inventory is not a cleanup receipt.
Cleanup stops every matching duplicate resource, preserves separately owned workspace/state data, and removes disposable resources by stable identity.
After settled creation and confirmed stop, authoritative absence can recover a lost removal acknowledgement.
Inventory exhausts pagination, rejects failed or cyclic scans, separates malformed resources, and revalidates ownership before mutation.

## Transport and resource guarantees

The SDK streams non-TTY stdin/stdout from startup and delivers stderr independently with a bounded queue.
Reads, writes, slow-consumer closure, and detachment have bounded cancellation behavior.
Closing transport releases its non-owning SDK handles; it does not establish environment termination.
Primary-process exit events retain their real code and scope.
Environment stop without a process code remains explicitly unknown through `ExitCodeKnown=false`, and no exit code or signal fabricates OOM evidence.

VM CPU and memory allocations have guest scope.
The guest workload PID budget is applied to the SDK's `microsandbox-workload` cgroup.
The private Docker daemon places nested containers below that cgroup, and whole-VM termination stops setup processes, the agent, background processes, and nested containers together.
This budget is distinct from a host VMM-thread limit and can be changed by guest-root code.

Ordinary execution uses cached layered images with no pulling.
Nested-container execution uses a flat ext4 root and permits bounded SDK materialization of the already selected exact registry digest.
Qualification found that cached layered metadata alone does not satisfy the SDK's flat-root materialization requirement.
There is no mutable image or backend fallback.

## Workspace and retained data

Local workspace seeds and logical attachments are materialized privately by the adapter and trusted workspace service.
The guest receives only its Participant subdirectories, with literal POSIX metadata and UID/GID 10001.
Capture requires an exact, unique managed environment, a matching source inode, and quiescence of every writer for the complete tar transfer.
Publication capture uses required guest writeback plus VM pause and bounded resume without waiting for the outstanding MCP call.
Terminal capture additionally requires a stopped environment and retained flush evidence.
A crash without that evidence provides unavailable corroboration, not a cleanliness claim.

The existing common capture service retains and validates sealed artifacts after environment removal.
`internal/runtime/dirstate` holds the shared protected-state and checksummed Session archive implementation previously owned by Docker.
Both backends validate ownership, backend binding, format, complete archives, and collection authorization.
Ordinary backups retain Session data, PostgreSQL, protected captures, and the native dispatch journal, excluding guest execution snapshots and disposable credential material.

Provider and control configuration are transferred through stdin into guest tmpfs before runtime launch.
They do not enter SDK sandbox configuration, labels, inventory, retained state, or ordinary error objects.
The authenticated MCP endpoint is supplied by the existing gateway and routed through the selected backend's network policy.
Real provider credentials remain accessible to guest code, as required by decision 0007.

## Qualification evidence

The KVM host runs Linux/amd64 and Docker Engine 29.7.2.
The outer qualification process runs as UID/GID 10001 with only explicit KVM device access and no host Docker socket in the guest.
The pinned development guest uses Docker 29.7.2, Compose, Go 1.27.0, and Testcontainers-Go 0.44.0 with Ryuk.

Observed passing scenarios during implementation include:

- The shared backend-neutral transport scenario: retry/conflict behavior, first-byte stdout, separate stderr, JSONL round trip, detachment, stop, removal, and revoked-epoch rejection.
- Fault and ownership coverage for lost creation responses, invisible ambiguous effects, failed stop/removal, replacement identities, pagination, duplicates, malformed resources, immutable inputs, and secret-free digests.
- Bounded closure with blocked reads/writes and a stalled stderr sink, plus positive primary-process exit handling.
- An authenticated outstanding HTTP call surviving required-flush capture, with preserved executable modes and symlinks.
- Fresh terminal capture, retained Session archive/export/restore, missing-state rejection, and storage isolation across fresh VMs.
- Both Roles' working-revision mise hooks and exact OpenCode Session Continuation across fresh environments using an imported digest-pinned golden image.
- Disposable native diagnostic initialization without Participant state.
- A non-root process running its private Docker daemon, Compose service networking, Testcontainers/Ryuk published-port checks, and confirmed VM stop while a nested container remains active.

The initial clean golden-image build exposed an unavailable pinned `pcre2-10.48-r0` APK.
The package lock now pins available `10.49-r0` artifacts with independently computed SHA-256 checksums for both supported image architectures.
OpenCode and mise runtime resource checksums remain the existing qualified values.

The explicit KVM gate is `mise run test-microsandbox-integration` with an approved Docker host and its numeric KVM group.
It builds fixture images outside Omnigrex, transfers the golden archive over stdin, runs race-enabled adapter tests, and removes the disposable wrapper.
The normal repository and Docker-backed integration gates remain required.
The final local gate passed formatting, static analysis, production reachability, race-detected unit tests, dependency-boundary checks, and both existing Compose validations.
The Docker integration gate passed doctor, ACP, Docker, PostgreSQL Store, and deployment suites on the local Docker Desktop daemon.
The full KVM gate passed every adapter test with the race detector, including both Roles' continuation, the subprocess doctor check with authenticated diagnostic MCP, and nested-container qualification.
The final native verification additionally checked unknown signal-exit normalization and rejection of overflowing SDK resource budgets.

Observed passing commands include:

```sh
mise run check
mise run test-integration
mise run test-microsandbox-integration
mise exec -- go vet -tags=microsandbox ./...
mise exec -- go test -race -tags=microsandbox ./internal/runtime/microsandbox ./internal/runtime/composition ./internal/doctor ./internal/config ./cmd/omnigrex
mise exec -- go test -race ./internal/runtime/microsandbox
git diff --check
```

The Microsandbox command used the operator-approved Linux Docker connection and its KVM group; no host address or deployment credentials are recorded here.
The native deadcode inspection reported only the same six existing redacted-formatting methods permitted by the repository's established gate.
The destructive Compose recreation test and live provider/GitHub gates were not run.
The final diff review confirmed that core Workflow and Agent Turn policy have no backend-specific changes.

## Review regression coverage

The first independent Standards and Spec reviews identified missing trusted Git tooling, digest-alias trust, conflicting ephemeral-input retries, missing-resource observation blocking recovery, unbounded SDK release, and malformed diagnostic inventory.
The native image now installs the checksum-locked Ubuntu Git/HTTPS dependency closure and tests actual trusted workspace preparation, capture inspection, and certificate handling in that image.
Image admission authenticates cached manifest/config bytes and verifies a selected platform manifest's linkage to its digest-pinned OCI index, rather than trusting a reference-shaped alias.
The pinned SDK reserializes registry manifests and omits parent indexes, so registry-origin/index verification requires bounded HTTPS access to the exact registry artifacts.
Exact imported platform-manifest artifacts remain verifiable without registry access when their cached bytes match their digest.

Ephemeral-input fingerprints remain process-local and are excluded from durable specification digests and records.
A separate random ownership nonce is retained in the private journal, allowing same-owner pre-start retries while rejecting conflicting inputs and registration adoption after control-plane restart.
The engine's original input snapshot is authoritative during preparation.
Recovery reports missing exit evidence as unknown and still reaches the existing authoritative absence barrier; unresolved creation remains blocked.
The worker-level regression exercises retained capture reuse and stopped acknowledgement after a lost removal response.
Malformed diagnostic expiry or contradictory Turn/diagnostic ownership is classified for exact-identity cleanup instead of aborting startup inventory.
Native exec close, stdin close, and sandbox detach waits are bounded, retain sanitized release failures, and never manufacture termination evidence.

The first corrected implementation passed the same local and both-backend gates.
The next independent review confirmed those fixes and identified three additional lifecycle regressions: terminal crashes were not recognized, failed writeback prevented force stop, and local journal failure could leave a pre-dispatch reservation unsettled.
The adapter now treats both SDK stopped and crashed states as authoritative environment-terminal observations without fabricating an exit code, OOM cause, or guest writeback.
Preparation-only terminal environments can be removed under positive NeverStarted evidence; started, unflushed environments retain their data and unavailable terminal corroboration.
Required writeback uses a shared finite budget that leaves time for identity-checked force termination, which receives its own finite cleanup budget after durable revocation.
Caller cancellation still prevents a successful cleanup receipt or Store acknowledgement.
The permanent suspended-VMM regression passed twice on the qualified KVM host, terminating the exact guest while withholding capture and removal authorization for unflushed work.
Fallible workspace-stat validation and journal publication now complete before main-phase admission.
An admission acknowledgement failure or backend image rejection known to precede native dispatch is durably settled as no execution effects, while genuinely ambiguous creation remains unknown.
The corrected journal-failure regression confirms zero native create calls and recoverable cleanup after storage becomes available again.
The following correctness review reproduced a journal-loss case where absent launch history had been interpreted as NeverStarted.
Launch history is now explicitly established only by fresh Store admission and retained trusted journal evidence.
A missing journal permits exact-resource termination but cannot authorize registration adoption, positive no-start evidence, or disposable resource removal.
The lost-journal regression covers both crashed and still-running executions, including a fresh flush that cannot replace the missing launch/capture provenance.

## Final review result

The change completed four independent Standards and Spec/Correctness review rounds.
All validated findings from the preceding rounds were corrected with focused regression coverage and relevant requalification.
The fourth Standards reviewer reported no remaining actionable findings after inspecting the complete tracked and untracked change set and affected callers.
The fourth Spec/Correctness reviewer independently reported no remaining actionable P1, P2, or P3 findings and confirmed the lost-journal evidence correction.
Both reviewers reran focused race-enabled checks; the correctness reviewer also reran the native-tag adapter/composition checks and `git diff --check`.

After the final implementation changes, `mise run check`, native-tag static analysis/race tests, `mise run test-integration`, and the complete race-enabled Linux/KVM `mise run test-microsandbox-integration` gate passed.
The KVM gate includes the permanent suspended-VMM force-stop regression, trusted Git preparation/TLS checks, digest-alias and OCI-index verification, both Roles' working-revision tooling and Session Continuation, MCP diagnostics, workspace/state lifecycle, and private Docker/Compose/Testcontainers qualification.
No implementation changes followed the clear fourth-round reviews.
The final documentation update records those observed results without expanding the verdict beyond Step 08's scope.

## Temporary compatibility and subsequent steps

OpenCode process rendering, golden-resource qualification, and diagnostic ACP remain transitional and are removed in Step 17.
The native SDK bridge, logical state/capture contracts, backend selection, ownership barriers, and shared conformance entry point remain.
Pi protocol/configuration/session integration, the full cross-backend failure matrix, published native deployment artifacts, and persistent KVM CI integration follow Steps 09-15.
