# Execution and agent boundary contracts

## Status and purpose

This is the Step 03 design for the [combined implementation plan](2026-10-05-pi-and-pluggable-execution-backends.md).
It defines the contracts implemented incrementally in subsequent steps; [Step 04](2026-10-07-docker-execution-boundary.md) supplies the first production Docker execution/recovery boundary.
The [Microsandbox](../experiments/microsandbox.md) and [Pi](../experiments/pi.md) experiments establish feasibility for the stated subset; they are not production adapters or a complete conformance suite.

The contracts below are normative about ownership and observable behavior.
Go signatures illustrate the intended interface boundaries; complete type declarations and private implementation details belong to their implementation steps.
Do not add unused production interfaces merely to make this document compile.

## 1. Decisions and scope

- [0005: Separate execution backends from coordination](../decisions/0005-separate-execution-backends-from-coordination.md).
- [0006: Use Pi through an agent-specific adapter](../decisions/0006-use-pi-through-an-agent-specific-adapter.md).
- [0007: Supply provider API keys through disposable files](../decisions/0007-supply-provider-api-keys-through-disposable-files.md).
- [0008: Keep workspace capture separate from publication policy](../decisions/0008-keep-workspace-capture-separate-from-publication-policy.md).
- [0009: Pin Participant images and use working-revision tools](../decisions/0009-pin-participant-images-and-use-working-revision-tools.md).

Retain Docker for trusted repositories and Microsandbox for Linux/KVM execution.
One deployment selects one backend; no fallback or mixed-backend routing is introduced.
The contract permits a future remote implementation without implementing a remote service now.
Pi is the only agent adapter after cutover, with temporary OpenCode compatibility during the staged refactoring.

Mariusz explicitly selected continued image pinning for retained Participants.
Compatibility qualification proves that a candidate can continue copied source state; it does not rewrite a Participant binding or migrate a live Session.
Each newly created Participant can select the current image, including a Participant created later within an existing Assignment Generation.
Retained Participants continue on their original images.

## 2. Ownership and dependency direction

| Owner | Responsibilities | Must not own |
| --- | --- | --- |
| Workflow, Agent Turn workers, and Store | Authority, leases, execution epochs, admission, retries, mutation reconciliation, retention authorization | Docker/Microsandbox requests, host filesystem layout, Pi commands |
| Neutral execution contracts | Identities, resource references, operation results, required capabilities, process streams | SDK clients, database queries, Workflow transitions |
| Concrete execution and storage adapters | Provisioning, actual process control, backend inventory, resource access, backend-specific failure translation | Selecting a Role, declaring a Workflow outcome, publishing to GitHub |
| Session coordinator | Fenced preparation and prompting, Control Owner checks, runtime session binding | Pi JSONL or vendor lifecycle APIs |
| Pi adapter | RPC, exact session reopening, configuration checks, History Replay, normalized Agent Events, owned MCP bridge | Lease ownership, direct GitHub authority, backend SDKs |
| Trusted workspace/publication service | Repository acquisition, Git validation, publication preconditions, terminal workspace comparison | Assuming a live workspace is a host directory |
| Composition layer | Constructing the selected adapters, management credentials, backend-specific configuration | Vendor branches throughout core workers |

Use `internal/runtime/execution` for backend-neutral contracts and `internal/runtime/agent` for agent-neutral contracts.
Keep `internal/runtime/profile` responsible for immutable configuration and qualification records.
Concrete backends remain under `internal/runtime/docker` and a future `internal/runtime/microsandbox`; the Pi implementation belongs under `internal/runtime/pi`.
The existing `internal/runtime/session` coordinator depends on the neutral agent contract.
The workspace service consumes neutral workspace/capture references; its trusted local staging paths remain private implementation details.

Neutral contract packages must not import Store, Workflow, a backend implementation, or an agent implementation.
Adapters receive the narrow authorization/storage dependencies they need through composition rather than importing the complete Store or manipulating Workflow rows.
Core workers may branch on a documented requirement or normalized outcome, never on `docker` versus `microsandbox`.

## 3. Identities, bindings, and authority

### 3.1 Separate identities

| Value | Meaning and lifetime |
| --- | --- |
| ExecutionKey | For Turn execution: installation namespace, Participant ID, Session ID, Turn ID, and execution epoch; immutable for one attempt to execute |
| OperationID | Durable idempotency identity for one provision, launch, capture, stop, or collection operation |
| EnvironmentRef / ProcessRef | Opaque backend-issued identifiers associated with an ExecutionKey and exact specification digest |
| WorkspaceRef | Logical prepared checkout, owner and seed revision; not a local path or a claim about its current HEAD |
| StateRef | Logical retained-state resource with Participant/Session ownership and a state-format version |
| CaptureRef | Immutable Workspace Capture associated with its operation and epoch |
| BackendBinding | Backend family and contract version plus the deployment's inventory/resource namespace |
| Runtime Profile binding | Immutable reference to the complete selected runtime/environment contract |

A reference is an identifier, not permission to act.
Every operation must be admitted under the applicable live Turn fence, recovered-execution cleanup fence, collection authorization, or bounded diagnostic admission.
Neither a correct UUID nor a matching resource label confers authority.
Backend management credentials stay on the trusted control plane and are never provided to repository workloads.

Persist the intended execution identity and immutable request digest before dispatching an operation that may create external resources.
An operation result must be committed only after revalidating its fence.
Operation IDs and execution epochs cross a future remote service boundary; in-memory handles are insufficient for restart recovery.
Long provisioning and transfer operations must permit lease heartbeats rather than depending on a database row lock held for the whole network call.
Losing the fence prevents adoption of the result and triggers exact-key recovery; it does not roll back external effects or prove that a remote request was cancelled.
Secrets are excluded from durable request digests, operation records, labels, and diagnostic values.
Excluding secret bytes does not authorize replacing another registration's ephemeral inputs: inputs remain bound to the exact admitted Turn and registration.

Doctor/readiness executions use a distinct diagnostic identity containing the installation namespace, probe ID, ownership, and expiry rather than fictitious Workflow/Turn IDs.
Their ephemeral sessions and MCP scopes cannot attach protected Session state or perform business mutations.
Inventory distinguishes these resources from Turn executions and applies diagnostic expiry/ownership cleanup rather than looking them up as missing Agent Turns.
This preserves read-only database preflight while allowing an actual disposable runtime/protocol check.

### 3.2 Immutable versus current configuration

The immutable binding includes the image digest and platform, agent runtime contract and qualified resource identity, backend contract, guest path layout, state format, and execution/security requirements.
It does not contain host volume names, host paths, bearer tokens, API keys, or a serialized effective instruction bundle.
Persist versioned canonical contract content so historical custom images can be resolved without rebuilding their identity from a mutable current configuration.

Current-deployment instructions continue to follow decision 0002.
Current repository Agent Profile configuration retains the default-branch provenance and selection rules in ADR 0011.
Model/thinking choices are checked against the retained runtime's actual capabilities on each Turn.
A new preference unsupported by that retained runtime fails explicitly; it does not select a newer image or another model silently.

Backend implementation release versions are recorded in qualification evidence and diagnostics.
A backend release must continue to implement all protected contract/storage versions it claims to support; a matching family name alone is insufficient.
Hardware and service upgrades that break that compatibility fail readiness rather than reinterpret stored references.

## 4. Execution contracts

### 4.1 Small capability interfaces

```go
type Environments interface {
    Prepare(context.Context, Operation, EnvironmentSpec) (EnvironmentRef, error)
    Launch(context.Context, Operation, EnvironmentRef, ProcessSpec, EphemeralInputs) (LiveProcess, error)
    Inspect(context.Context, EnvironmentRef) (EnvironmentObservation, error)
    EnsureStopped(context.Context, Operation, ExecutionKey) (StopReceipt, error)
    EnsureAbsent(context.Context, Operation, ExecutionKey) (RemovalReceipt, error)
}

type Inventory interface {
    Resolve(context.Context, ExecutionKey) (Resolution, error)
    List(context.Context, InventoryCursor) (InventoryPage, error)
}

type Diagnostics interface {
    Describe(context.Context) (BackendDescription, error)
    Check(context.Context, Requirements) (CheckReport, error)
    CheckImage(context.Context, ImageArtifact) error
}
```

`Operation` carries an operation ID, exact owner/epoch, specification digest, and the applicable admission scope.
`EnvironmentSpec` declares an exact image/platform, logical workspace and state attachments, bounded resources, required isolation/features, and trusted bootstrap requirements.
`ProcessSpec` declares the fixed executable/arguments, working directory, numeric identity, and nonsecret environment produced by the agent/environment adapter.
Repository configuration cannot supply arbitrary host mounts, devices, management-network settings, or backend labels through these structures.
Use an absolute qualified runtime executable and separate tool activation from protected agent configuration.
Repository tool provisioning must not override control variables such as `PI_*`, `OMNIGREX_*`, `NODE_OPTIONS`, or loader injection settings when the Runtime Process is launched.

`Prepare` may boot the environment and run repository tool provisioning inside it, but does not submit a model prompt or grant MCP mutation authority.
The environment service compiles its setup commands from platform policy and the working revision; Docker and Microsandbox do not independently interpret repository mise policy.
Image fetching, if required by a backend's configured provisioning path, must use the exact declared digest and remain inside the bounded preparation operation.
Step 04 preserves the existing Docker image-availability behavior rather than adding pulling as an incidental refactor.

`Launch` installs bounded ephemeral inputs and starts the Runtime Process with stdout captured from its first byte.
It returns the process reference and separate stdin, stdout, and stderr streams without a TTY.
If a backend cannot attach before starting, it must provide equivalent bounded startup buffering and report overflow rather than lose protocol records.
This is process launch, not authorization to prompt.

Closing the transport only detaches/closes streams; it is never a stop receipt.
Agent-specific cancellation is separate from environment termination.
All potentially blocking stream operations and lifecycle calls need bounded cancellation behavior, including blocked writes and slow consumers.

### 4.2 Retry and ambiguity rules

- The same operation ID and specification returns the same logical result once completed.
- Reuse with different nonsecret content is a conflict, not an update.
- A timed-out create/launch is `UnknownEffect` until resolved; do not issue a new execution identity just to escape the timeout.
- Resolve returns `Unique`, `Duplicate`, `Absent`, or `Unknown`, with exact ownership evidence.
- Duplicates are reported to core recovery, not silently reduced to whichever instance answered first.
- A stale epoch cannot launch, replace mounts, restore state, or remove a newer epoch's resources.
- A lost launch response does not authorize adopting an already running prompt after orchestrator restart.

`EnsureStopped` succeeds only after every workload belonging to the key has ceased executing, including nested containers, background processes, and setup processes.
Stopping only Pi is insufficient.
It preserves workspace and retained data needed for capture/recovery.

`EnsureAbsent` additionally removes disposable execution resources and preserves separately owned retained state and protected captures.
It cannot succeed solely because a list response is empty.
Outstanding ambiguous operations must be settled or fenced so a delayed start cannot resurrect the old execution after cleanup succeeds.
An adapter may use immutable resource IDs, an operation ledger, or backend-side revocation to establish this; a provider unable to establish it does not meet the contract.

Inventory is paginated and scoped to the installation's managed resources.
Partial scans and unavailable pages are explicit failures/unknown observations.
Malformed managed resources are returned separately and can be removed only after the adapter revalidates management ownership and exact identity.
Unrelated resources must never be selected by a broad name or label prefix.

### 4.3 Observations and errors

Normalize environment state as `Preparing`, `Ready`, `Running`, `Stopped`, `Absent`, or `Unknown`.
Exit observations identify their resource scope, timestamp, optional exit code, and optional cause with provenance.
An exit code of 137, EOF, or a lost connection does not prove OOM.
A backend may report an unknown cause; it must not manufacture Docker-equivalent facts it cannot observe.
Shared controller failure must not be attributed to one guest process without evidence.
Resource budgets state their units and enforcement scope; a container PID limit, a VM's guest process count, and its host VMM thread count are not interchangeable guarantees.
Reject a specifically required limit that cannot be enforced instead of relabeling an approximate guest observation as host-enforced isolation.

Use common failure classes: `InvalidSpec`, `UnsupportedRequirement`, `BindingMismatch`, `Conflict`, `NotFound`, `Unavailable`, `DeadlineExceeded`, `UnknownEffect`, `CorruptArtifact`, and `OwnershipMismatch`.
Separate whether a request can be retried from whether its effect is known.
`NotFound` is authoritative only for the exact queried resource and does not prove the absence of an unresolved creation.
Core policy maps these classes onto existing retry/recovery/handoff behavior; vendor SDK errors never determine Workflow transitions directly.

### 4.4 Capabilities without vendor switches

Both backends must satisfy the lifecycle, transport, capture, state ownership, and recovery floor.
The initial requirements distinguish process/container isolation, hardware-isolated execution, in-environment tool installation, and nested Docker/Compose/Testcontainers support.
These describe guarantees of the selected backend plus image and policy, not marketing labels on an image.

Docker is qualified for trusted repositories and need not provide nested Docker or a microVM boundary.
Microsandbox must qualify the stronger isolation and nested-container requirements.
A repository requiring nested-container tests cannot run on an incapable Docker configuration just because Docker is installed on the host.
Refuse before launching repository work; never grant the guest the host Docker socket as a fallback.
Optional diagnostics may be unavailable, but mandatory safety and consistency properties cannot be optional capabilities.

## 5. Workspace and retained-state contracts

### 5.1 Workspace access

```go
type Workspaces interface {
    Prepare(context.Context, Operation, WorkspaceSeed) (WorkspaceRef, error)
    Capture(context.Context, Operation, CaptureRequest) (CaptureRef, error)
    OpenCapture(context.Context, CaptureRef) (CaptureManifest, io.ReadCloser, error)
    Discard(context.Context, Operation, WorkspaceRef) error
    ReleaseCapture(context.Context, Operation, CaptureRef) error
}

type ToolStorage interface {
    Prepare(context.Context, Operation, ToolPathRequests) (ToolBindings, error)
    Usage(context.Context, ToolStorageRef) (UsageObservation, error)
    Collect(context.Context, Operation, ToolStorageRef) error
}
```

The trusted repository service fetches the authorized revision without exposing GitHub credentials to the guest, then supplies a bounded seed artifact to workspace `Prepare`.
The backend verifies the seed identity and reserves the logical checkout reference.
Physical materialization may occur immediately or when environment `Prepare` attaches that reference; the environment cannot become ready or run repository setup until the exact seed is materialized.
Capture of a seed-only or partially materialized workspace fails explicitly.
Repository scripts and tools execute inside the prepared environment, not while acquiring the trusted seed.

Turn scratch, Participant tool caches, workspaces, captures, and Retained Runtime State have separate ownership and retention rules.
Tool path requests remain logical names/lifetimes and permitted environment bindings, not repository-provided absolute host paths.
Usage observations retain the current distinction between complete measurements and truncated/unavailable scans.

### 5.2 Capture consistency and trust

A successful capture represents one consistent workspace revision with immutable content and provenance: owner, Turn/epoch, operation, purpose, revision identity, manifest version, byte limits, and content digest.
`OpenCapture` serves that same content after the original Runtime Process exits and after orchestrator restart for as long as its retention authorization protects it.
An already sealed capture cannot be silently replaced by a later observation when a caller retries after losing the response.

The capture implementation is part of the trusted control plane.
It must quiesce all workspace writers while sealing a copy, use an equivalent consistent storage snapshot, or fail explicitly.
Two best-effort reads of a writable directory are not proof of a consistent capture.
A JSON `clean: true` supplied by repository code or the agent is not evidence.

During an MCP publication call, Pi may be waiting for that call's result.
Capture must not wait for the current prompt to settle, require the paused agent to export its own files, or destroy the session merely to obtain a consistent view.
Backend-private suspension/snapshot mechanics must keep the RPC/MCP interaction viable and remain bounded by the operation and Turn deadlines.
Steps 01-02 did not prove atomic workspace capture.
Start Step 05 with a focused mid-prompt capture experiment on both backends before committing to the artifact implementation; the complete Microsandbox adapter later repeats that gate in Step 08.

The portable artifact must contain enough original Git objects, index/HEAD and relevant metadata, tracked file contents/modes/symlink targets, and working-tree inventory to verify the existing publication policy.
Ignored build caches may omit content only where that cannot hide staged changes, dirty tracked files, or non-ignored untracked files.
The common trusted inspector, not vendor-specific policy code, determines those semantics.
Fix the versioned artifact encoding and extraction limits in Step 05 before adding both adapter implementations.

Treat received bytes as untrusted input even when the transfer service is trusted.
Verify manifest ownership/digests/completeness, reject traversal and unsupported entries, never follow captured links outside staging, and neutralize repository hooks, configuration, object alternates, grafts, replacement refs, and shallow-history tricks before invoking trusted Git.
Preserve the existing first-parent, merge-parent, tree, file-mode, symlink, and remote-head checks.
Capture transport may use local copying or remote streaming; neither a host path nor a vendor download URL reaches core coordination.
Local staging/caching optimizations remain private to the trusted workspace service.

### 5.3 Publication and terminal corroboration

1. Reserve the MCP mutation under the current fence and allocate its capture operation ID.
2. Obtain and verify the publication capture.
3. Validate and stage its proposed commits in the orchestrator-owned publication checkout.
4. Record the exact proposed tip before dispatching the GitHub push.
5. Preserve the existing branch precondition and uncertain-publication reconciliation rules.
6. At Turn finalization, close mutation admission, drain/reconcile outstanding mutations, and confirm environment stop.
7. Take a fresh terminal capture before destroying data required for that observation.
8. Compare that capture with the trusted published revision, preserving dirty/unpublished-work detection.
9. Retain the terminal capture until durable settlement or deferred terminal corroboration no longer needs it.

The publication capture cannot stand in for the terminal capture: a process may edit files after publishing.
A lost or unavailable terminal capture is unavailable corroboration, not proof that the workspace matches.
Delayed corroboration consumes the retained capture reference, not whichever checkout happens to occupy the Participant's old directory.
Reviewer discard and other cleanup follow Role Policy after the required observations; the backend does not decide which findings or outcomes count.

### 5.4 Retained state

```go
type RetainedState interface {
    Ensure(context.Context, Operation, StateOwner, StateContract) (StateRef, error)
    Inspect(context.Context, StateRef) (StateObservation, error)
    Export(context.Context, Operation, StateRef) (StateArchive, error)
    Restore(context.Context, Operation, StateArchive) (StateRef, error)
    Collect(context.Context, Operation, StateRef) error
}
```

State ownership is Participant/Session-specific even if an adapter stores several resources in one physical volume.
Attaching writable state requires an exclusive, epoch-bound writer authorization.
Gateway token revocation does not revoke filesystem access; confirm the prior environment stopped before granting a new writer.
An old cleanup operation cannot delete a new workspace or state attachment by reusing a directory name.

Export requires a quiescent writer boundary and participates in the operator guide's coordinated database/state recovery set.
That recovery set must also include protected Workspace Captures referenced by unresolved settlement or corroboration records, not just the agent session files.
Capture export uses the sealed manifest/content contract; a missing protected capture makes the recovery set incomplete.
The archive records logical ownership, runtime/state contract identity, format version, checksums, and completeness without credentials or executable environment snapshots.
Restore validates the complete artifact into staging before publishing the new state reference.
Partial transfers are never considered restored state.
Backend-private location metadata belongs in protected recovery metadata, not source-controlled compatibility examples.
Restoring to a compatible host with the same backend contract is supported; cross-backend conversion is not implied by this API.

Collection follows Store authorization and is idempotent by exact resource reference.
It cannot collect state protected by retained Sessions, unresolved mutations/corroboration, an active writer, or a coordinated backup operation.
Workspace/tool caches remain reconstructible where existing policy permits; protected captures needed for recovery are not ordinary disposable caches.

## 6. Agent and MCP contracts

```go
type AgentAdapter interface {
    ProcessSpec(AgentConfiguration) (ProcessSpec, error)
    Connect(context.Context, Transport, SessionIdentity, AgentConfiguration) (AgentConnection, error)
}

type AgentConnection interface {
    Prepare(context.Context) (SessionReady, error)
    Configure(context.Context, ModelSelection) (EffectiveConfiguration, error)
    Replay(context.Context, HistoryCursor, AgentEventSink) (HistoryCursor, error)
    Prompt(context.Context, AuthorizedPrompt, AgentEventSink) (PromptOutcome, error)
    Cancel(context.Context) error
    Close() error
}
```

The session coordinator owns the authorization implied by `AuthorizedPrompt`; callers cannot bypass its fence by constructing a raw Pi command.
Public human/agent clients do not receive an RPC passthrough API, including Pi's separate `bash` command.
`Close` disconnects the adapter; execution termination remains an environment operation.

For Pi, derive an exact stable runtime session ID from the durable Agent Session ID and use its isolated state reference.
Do not select the latest session, a partial ID, or an arbitrary matching file.
The ID must be known before launch so an ambiguous creation response does not require guessing which conversation was created.
Keep Session Continuation separate from History Replay.
Replay uses persisted entries and a durable cursor, emits past Agent Events, and must not call the provider.
Current-branch context and complete append-order history are not interchangeable, especially after compaction or branching.

`Prepare` verifies runtime/session identity, required RPC features, and successful authenticated MCP discovery before prompt admission.
The initial production design uses the qualified owned extension; native Pi MCP remains an alternative requiring equivalent qualification rather than an assumed replacement.
The MCP gateway returns an agent-neutral endpoint/authentication descriptor; the Pi adapter renders its connection details.
Remove `acp.MCPServer` from the gateway's public contract during the Pi transition.

Model selection must match the exact requested provider/model ID.
Thinking selection must be supported and verified after application because Pi otherwise silently clamps values.
Missing credentials, unsupported values, and discovery failures are explicit errors, not fallback requests.
No Omnigrex model catalog is introduced.

The adapter continuously reads stdout and correlates command responses by ID while separately delivering asynchronous events.
Normalize outcomes as settled completion, provider/runtime failure, abort, or unknown execution after transport/process loss.
For Pi, successful prompt admission is not completion; wait for final `agent_settled` and inspect the final assistant outcome.
Intermediate `agent_end` events and retries cannot settle an Omnigrex Turn.
Runtime retries remain bounded within the Turn deadline; durable infrastructure and mutation retries remain in the orchestrator.

Agent Events and diagnostic context use runtime-neutral identifiers and bounded, redacted metadata.
Errors must not retain secret-bearing source objects through wrapping or debug formatting.
Human-controlled prompting preserves the existing exclusive ownership and replay-before-prompt requirement.
This preserves the internal human prompt/replay capability; a new operator UI or human execution-launch workflow is not delivered by this work.

## 7. Image selection and in-environment tooling

Use a repository environment definition from the same exact default-branch snapshot used for trusted Agent Profile discovery, with the deployment golden image as the default when absent.
The proposed file location is `.omnigrex/environment.yaml`; Step 07 fixes its strict versioned schema alongside the existing turn-configuration document rather than overloading Agent Profile identity.
Its scope is an exact prebuilt image and declared environment requirements, not backend selection or host access.

Bind the selected environment image/platform when each Participant is first prepared, using an atomic Store operation.
Later Participants in the same Generation select from their own current default-branch configuration snapshot.
An existing Developer can therefore use image A while a newly created Reviewer uses image B.
This is the explicitly selected per-Participant policy, not an implicit runtime upgrade.
Each Participant receives its own immutable Runtime Profile, Role permissions, workspace, and state references.

Tool installation and tests use each Turn's working revision inside the environment.
This removes the Reviewer/default-branch mise split without moving Agent Profile or image-selection authority to a Change Proposal.
Image ancestry or a claimed Pi version is not sufficient qualification; verify the exact artifact against the golden-image/runtime resource contract and exercise its protocol/configuration exclusions.
Repository-selected images are still guest code, not authorities that can grant GitHub or Workflow capabilities.

Normal deployment upgrades do not rewrite retained bindings.
Keep their exact images and historical contract definitions available until retention permits collection.
Qualify source-to-candidate Pi continuation and replay on isolated copies for each required backend/platform contract.
Qualification is not an in-place migration command and does not qualify a reverse downgrade.
An unavailable old image is an explicit protected-binding failure, not permission to launch its Session on the current image.

## 8. Existing integration points and migration owners

This inventory covers the current production Docker imports, indirect filesystem/storage coupling, protocol coupling, and operational entry points at baseline `e66418a`.
It also identifies where the new experimental evidence belongs.

| Current location | Coupling | Target owner / step |
| --- | --- | --- |
| `cmd/omnigrex/main.go`, including `startupDockerAdapter` | Concrete factories, inventory translation, readiness, cleanup and cache-monitor wiring | Composition only; move vendor translation into adapters in Step 04, add selected-backend wiring in Step 08 |
| `internal/agentturn/runtime.go` | Docker Spec/EngineOptions/ExitObservation, volume maps, OpenCode paths and ACP | Neutral environment handles in Step 04; workspace/state ports in Steps 05-06; agent adapter in Steps 09-12 |
| `internal/agentturn/stop_worker.go` | Docker labels and container OOM identity | Exact ExecutionKey cleanup and neutral evidence in Step 04 |
| `internal/startup/reconciler.go`, `monitor.go` | Container-named identifiers and inventory-driven recovery | Neutral resource refs; keep Store-authoritative recovery policy in Step 04 |
| `internal/runtime/docker/runtime.go` | Docker API, attach demultiplexing, restrictions, volume subpaths, stop/remove | Retained concrete backend implementation of execution ports in Step 04 |
| `internal/runtime/docker/inventory.go`, `exact_label_cleaner.go` | Label parsing, duplicate/malformed handling, exact cleanup | Docker-private implementation of inventory and cleanup contracts in Step 04 |
| `internal/runtime/docker/oneshot.go`, `assignment_runtime_state_cleaner.go` | Helper containers and path-based state deletion | Backend-private implementation of state/storage ports in Step 06 |
| `internal/runtime/docker/readiness.go`, `image_availability.go` | Docker resource and exact-image inspection | Backend diagnostics/image validation in Steps 04 and 15 |
| `internal/runtime/opencode/process.go` and render/configuration helpers | Agent configuration compiled straight to Docker policy | Transitional OpenCode adapter emitting neutral process configuration in Step 04; remove in Step 17 |
| `internal/runtime/profile/*.go` | OpenCode-only contract validation, path layout and state qualification constants | Neutral immutable contracts and image/state axes in Steps 06-07; Pi qualification in Step 14 |
| `internal/agentturn/preparer.go` and `internal/agentprofile` | Runtime resolution, current profile selection and source provenance | Per-Participant environment selection and immutable binding resolution in Steps 06-07; preserve ADR 0011 |
| `internal/workspace/lifecycle.go`, `workspace_swap.go`, `git.go` | Host roots, epoch-aware directory replacement, local checkout acquisition | Trusted seed/publication service plus backend-local workspace implementation in Step 05 |
| `internal/workspace/publish.go`, `publication_history.go`, `tree*.go`, `reconcile.go` | Live local workspace inspection, Git import and publication corroboration | Shared trusted capture inspection and publication policy in Step 05; keep remote GitHub reconciliation here |
| `internal/agentturn/execution_worker.go`, `outcome_reconciler.go`, `terminal_corroboration_worker.go`, `publication_recovery.go` | `workspace.Paths`, direct tree comparison and post-cleanup observations | Capture references and staged stop/capture/remove finalization in Step 05 |
| `internal/workspace/mise.go`, `mise_epoch.go` | Orchestrator-side repository tool installation and path rewriting | Environment preparation inside the selected backend in Step 07 |
| `internal/turnconfig`, `internal/workspace/turn_paths.go` | Logical path requests currently rendered into fixed local mount paths | Retain requests/allowlists; backend tool-storage bindings in Steps 06-07; reserve Pi control variables |
| `internal/workspace/tool_cache*.go`, `internal/retention/tool_path_cleaner.go` | Local scans and deletion of Participant caches | Backend storage/usage ports with complete-versus-truncated observations in Step 06 |
| `internal/retention/worker.go`, `internal/store/retention.go` | Cleanup targets represented as canonical runtime-state paths | Logical state/capture resource references and existing collection authorization in Step 06 |
| `internal/store/runtime_oom.go`, `startup_recovery.go`, `turns.go`, `settlement.go`, `mutation_recovery.go` | Container-specific evidence and runtime identity fields | Neutral persisted identities/evidence; retain fencing and settlement semantics in Steps 04-06 |
| `internal/store/preparation.go`, `runtime_profile_compatibility_results.go` and migrations | Image/profile bindings, ACP session IDs, state paths, qualification rows | Forward-only transitional schemas in Steps 06 and 11; exact qualification provenance in Step 14 |
| `internal/runtime/session`, `internal/runtime/acp`, `internal/runtime/agentevent` | ACP request/response types and session identifiers | Agent-neutral coordination/events with Pi-specific protocol internals in Steps 09-11 |
| `internal/mcp/gateway.go` | Registration exposes `acp.MCPServer` | Neutral authenticated endpoint descriptor in Step 10 |
| `internal/mcp/backend.go` | WorkspacePublisher dependency | Preserve capability policy; consume trusted capture-based publication service in Step 05 |
| `internal/role/policy.go` | OpenCode hardening and Reviewer trusted-tools revision policy | Agent-neutral constraints plus Pi translation; working-revision provisioning in Steps 07 and 10 |
| `internal/doctor/preflight.go`, `acp.go`, `mcp.go` | Direct Docker/ACP construction and probes | Selected-backend diagnostics plus agent preflight in Steps 04, 08 and 15 |
| `internal/config/config.go` | Docker network/volume roots and OpenCode image settings | Namespaced selected-backend configuration at composition; remove obsolete agent settings at cutover |
| `Dockerfile`, `agent/opencode/`, `compose.yaml`, `deploy/compose.yaml`, both environment examples | Docker-only resources and agent distribution | Dual backend deployment and qualified Pi/environment artifacts in Steps 07 and 15; OpenCode removal in Step 17 |
| `.github/workflows/ci.yaml`, `mise.toml`, integration tests under runtime/doctor/store/deployment | Docker-only image, qualification and test entry points | Shared conformance factories plus backend-specific jobs in Steps 04, 08, 13-15 |
| `README.md`, `docs/operator-guide.md`, compatibility documentation | Docker-specific readiness, backups, restore, image retention and deployment procedures | Selected-backend operational procedures in Steps 15-16; link rather than duplicate secret procedures |
| `experiments/microsandbox`, `experiments/pi` | CLI prototypes and controlled/live feasibility fixtures | Evidence and reusable scenarios, not production dependency exemptions |

Historical migrations remain unchanged; add forward-only migrations.
Existing Docker/ACP tests remain active while their production paths remain active.
Do not implement an OpenCode session conversion under the guise of a transitional schema.

## 9. Conformance and dependency checks

The same test functions accept a backend factory and operate on neutral ports.
Docker and Microsandbox must both pass the core suite; backend-specific requirements have additional tests rather than weakened core assertions.
Use a small controlled process before involving Pi, then run the agent suite across both factories.

| ID | Required proof | First delivery |
| --- | --- | --- |
| E01 | Exact image/spec validation; unsupported requirements rejected before execution | Step 04, Microsandbox in Step 08 |
| E02 | Same-key retry creates one logical execution; changed request conflicts | Steps 04, 08 |
| E03 | Lost create/start responses resolve safely; delayed old starts cannot outlive a successful cleanup barrier | Steps 04, 08, full fault matrix in Step 13 |
| E04 | Non-TTY streams preserve framing, stderr separation, EOF, bounded writes and slow-reader behavior | Steps 04, 08 |
| E05 | Cancellation/transport loss differs from confirmed stop; descendants and nested containers stop | Steps 04, 08 |
| E06 | Restart inventory handles pagination, duplicates, malformed resources and unrelated resources safely | Steps 04, 08 |
| E07 | Exit cause unknown is preserved; positive OOM evidence is scoped and recorded before removal | Steps 04, 08 |
| W01 | Seed materialization works with no shared host filesystem; GitHub credentials never enter the guest | Step 05 |
| W02 | Capture is consistent while writers race; unavailable consistency fails rather than publishing | Both-backend feasibility at the start of Step 05; adapter gates in Steps 05 and 08 |
| W03 | Mid-prompt capture completes without waiting on its own MCP response; channel remains usable | Both-backend feasibility at the start of Step 05; adapter gates in Steps 05 and 08 |
| W04 | Dirty files, late edits after publication, missing inventory, invalid history and unsafe paths are rejected | Step 05 |
| W05 | Terminal capture remains available through restart/deferred corroboration after environment removal | Steps 05-06 |
| S01 | Exclusive state writer, fresh-process continuation, and stale cleanup isolation | Step 06, Microsandbox in Step 08 |
| S02 | Complete checksummed backup/restore and rejection of partial, corrupt or mismatched state | Steps 06, 14 |
| S03 | Protected resources resist collection; scratch/cache lifetimes and incomplete usage scans remain distinct | Step 06 |
| A01 | Exact session identity, strict model/thinking configuration, replay without prompting | Steps 09-11 |
| A02 | Admission versus settlement, errors/aborts/retries, current instructions and authorized MCP bridge | Steps 09-12 |
| A03 | No secret inputs in control records or normal persisted session output; per-Turn MCP revocation | Steps 10-13 |
| Q01 | Historical bindings remain pinned; new Participants select candidates, including within an existing Generation; qualification never mutates source state | Steps 07, 14 |

Add Go import-graph checks for production packages, including platform/build-tag variants supported by the project.
Forbid core imports of backend SDKs/implementations and Pi/ACP protocol packages after their migration step.
Forbid neutral public signatures from exposing vendor types, local host paths, Docker labels, or untyped vendor option maps.
Allow concrete constructors in a small composition/factory package, not throughout doctor and worker code.
Keep backend-specific adapter tests and disposable experiments outside that production rule, with explicit path-based scope rather than symbol-by-symbol exemptions.
Run the normal repository gate and the applicable shared/backend integration suites at each implementation milestone.

## 10. Remote-backend walkthrough

| Scenario | Contract behavior | Core change needed for a conforming remote adapter |
| --- | --- | --- |
| Repository checkout is on another machine | Trusted acquisition supplies WorkspaceSeed; adapter materializes it remotely | None |
| Provider creates an environment but the response is lost | Resolve the persisted operation/key; fence ambiguous old effects before retrying | None |
| Remote process streams Pi output | Adapter transports separate byte streams; Pi adapter sees the same records | None |
| Agent calls `publish_changes` remotely | Trusted capture is streamed and inspected locally; publication still uses orchestrator credentials | None |
| Agent edits after publishing | Fresh stopped-environment terminal capture detects the difference | None |
| Connection disappears during stop | Return unknown; keep recovery barrier and deny a replacement state writer | None |
| Orchestrator restarts | Inventory and durable refs identify resources without an in-memory handle; do not adopt prompts | None |
| Session continues later | Attach its exact StateRef to a new environment using its pinned runtime binding | None |
| Restore onto another compatible host | Restore the coordinated recovery set and explicitly remap backend locations, with the original host fenced off | No Workflow/agent change; backend and operator restore integration required |
| Provider cannot capture consistently or confirm termination | Backend cannot qualify for the contract | Do not weaken core behavior to accommodate it |

A remote implementation still needs management authentication, artifact transfer, provider lifecycle integration, and a reachable authenticated MCP route or transport bridge.
Those are adapter/deployment obligations, not features supplied automatically by defining Go interfaces.
Backend-native logs, snapshots, and download URLs stay behind that implementation.

## 11. Delivery boundary and unresolved implementation details

Step 03 completes the semantic design and mapping; Step 04 begins the production Docker refactoring.
The contracts describe the final boundary, but implementation is staged: Step 04 removes direct Docker coupling while Steps 05-06 replace live workspace paths and path-based retained-state ownership.
Preserve the functioning OpenCode/Docker route during those steps, with explicit transitional adapters rather than pretending the full target contract has already been delivered.
The precise capture mechanism, versioned archive encoding, Microsandbox SDK-versus-CLI choice, and deployment resource defaults must be proved in their assigned implementation steps.
They may refine private mechanics but cannot silently weaken consistency, ownership, transport, or termination guarantees.
If the selected backend cannot satisfy a mandatory property, record the failed proof and revisit the design before claiming its conformance.

The per-Participant image selection and retained-image pinning policies are recorded in decision 0009.
No additional human approval is required for routine interface names or private helper layout.
No production code, database schema, running deployment, or existing accepted first-iteration specification is changed by this design document.
