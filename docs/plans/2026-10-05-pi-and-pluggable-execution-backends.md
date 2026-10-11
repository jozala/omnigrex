# Pi runtime and pluggable execution backends

## Objective

Replace OpenCode with Pi while introducing a backend-independent execution boundary with retained Docker and Microsandbox implementations.

Initially implement local execution, but keep workspace access, retained state, process management, and recovery independent of shared host filesystems so a future remote backend does not require changes to Workflow decisions or Agent Turn coordination.

Deliver the work sequentially, with executable evidence at each step.

## Related work

- [Replace OpenCode with Pi RPC as the sole agent runtime](https://github.com/jozala/omnigrex/issues/33).
- [Migrate Agent Turn isolation from Docker containers to Microsandbox](https://app.clickup.com/t/124ckhdev73).

This combined plan revises the delivery sequence and overlapping infrastructure assumptions of those Work Items.
When implementation begins, update their descriptions to link this plan and identify superseded requirements.

## Recorded milestones

| Step | Result | Evidence or deliverable |
| --- | --- | --- |
| 01 | Linux execution topology proved | [Microsandbox feasibility evidence](../experiments/microsandbox.md) |
| 02 | Required Pi RPC, MCP, session, and API-key gates passed on both backends | [Pi feasibility evidence](../experiments/pi.md) |
| 03 | Contracts and migration ownership defined; production implementation follows | [Execution and agent boundary contracts](2026-10-07-runtime-boundary-contracts.md) |
| 04 | Docker launch and recovery use neutral contracts; OpenCode remains operational | [Docker execution boundary implementation and evidence](2026-10-07-docker-execution-boundary.md) |
| 05 | Logical workspace preparation, sealed publication evidence, and retained terminal corroboration | [Location-independent workspace implementation and evidence](2026-10-08-location-independent-workspaces.md) |
| 06 | Logical retained-state bindings, protected collection, and validated offline Session archives | [Retained-state implementation and evidence](2026-10-08-retained-state-bindings.md) |
| 07 | Digest-pinned repository environments, persisted contracts, and working-revision guest tooling | [Repository environment implementation and evidence](2026-10-08-repository-environments.md) |
| 08 | SDK-backed Linux/KVM execution, shared protocol conformance, capture/state bindings, and nested-container qualification | [Microsandbox backend implementation and evidence](2026-10-09-microsandbox-backend.md) |
| 09 | Correlated Pi RPC, admission/settlement outcomes, bounded cancellation, and disposable both-backend diagnostics | [Pi protocol adapter implementation and evidence](2026-10-09-pi-protocol-adapter.md) |
| 10 | Checked Pi configuration, owned authenticated MCP, current instructions, authorized checkout-local skills, and ephemeral API keys | [Pi configuration and MCP implementation and evidence](2026-10-09-pi-configuration-and-mcp.md) |
| 11 | Reserved exact Pi identity, fenced durable preparation, fresh-environment continuation, and bounded append-order History Replay | [Durable Pi Session implementation and evidence](2026-10-09-durable-pi-sessions.md) |
| 12 | Candidate Pi Runtime Profile, common complete Agent Turns, and controlled Developer-to-Reviewer Workflows on both backends | [Complete Pi Agent Turn implementation and evidence](2026-10-09-complete-pi-agent-turns.md) |
| 13 | Shared eleven-scenario Pi recovery qualification and cross-target production dependency enforcement | [Pi failure recovery and boundary independence](2026-10-10-pi-failure-recovery.md) |
| 14 | Backend-scoped Pi qualification artifacts, isolated source-to-candidate continuation/replay, and protected-state recovery | [Pi upgrades and state recovery](2026-10-10-pi-upgrades-and-state-recovery.md) |
| 15 | Backend-selected Pi diagnostics/readiness, qualified image-only bundles, and required KVM CI/publication wiring; release execution pending | [Pi operational integration and backend CI](2026-10-11-pi-operational-integration.md) |

The contract document and its linked decisions refine this plan's architectural outline for Steps 04 onward.

## 1. Agreed scope

### Execution backends

- Retain Docker and Microsandbox as supported implementations.
- Select one backend per deployment.
- Support Docker for trusted repositories.
- Initially support Microsandbox deployments on Linux with usable KVM.
- Keep Docker-backed development available on macOS.
- Run the same core conformance suite against both backends.
- Explicitly distinguish backend guarantees and supported capabilities.
- Reject requirements a selected backend cannot satisfy.
- Do not silently fall back to another backend.

### Agent runtime

- Pi becomes the only coding-agent runtime after cutover.
- Isolate Pi-specific protocol and session behavior behind a narrow adapter.
- Permit Pi-specific Agent Profile configuration where appropriate.
- Preserve Omnigrex ownership of session control, Turn fencing, Workflow transitions, and MCP authority.
- Preserve Session Continuation and History Replay as distinct operations.

### Development environments

- Accept prebuilt, digest-pinned repository images derived from an Omnigrex golden image.
- Build those images outside Omnigrex.
- Install additional repository tools inside the execution environment.
- Use tool configuration from the actual working revision for both Developer and Reviewer.
- Preserve separate Role authority, workspace isolation, and Reviewer workspace discard.
- Require Docker, Compose, and Testcontainers support inside Microsandbox Agent Turns.
- Run Microsandbox-specific integration tests on a qualified KVM CI host.
- Investigate nested microVM execution, but do not make it a release gate.

### Credentials

- Initially provide real provider API keys ephemerally on both backends.
- Keep credentials out of retained session state, ordinary runtime-state backups, and diagnostics.
- Defer provider credential mediation and a credential proxy.
- Preserve per-Turn authenticated MCP authority.
- Never provide GitHub App credentials to the agent environment.

Supplying real provider credentials means microVM isolation does not prevent guest code from accessing those credentials.
The operational trust description must reflect this.

### Cutover

- Keep the existing OpenCode deployment usable during preparatory work.
- Perform one explicit clean cutover to Pi.
- Do not migrate OpenCode conversations or populated operational state.
- Qualify later Pi-to-Pi upgrades against retained Agent Sessions on both backends.
- Keep retained Participants on their exact images; qualification tests copies of source state and does not authorize rebinding or in-place migration.
- Select the current repository image independently for each newly created Participant, including later Participants in the same Assignment Generation.
- Do not implement automatic Docker-to-Microsandbox state migration.

Changing the configured backend must not reinterpret retained state belonging to another backend.
A backend change requires explicit state disposal or a separately implemented migration.

## 2. Architectural contract

### 2.1 Ownership

| Component | Owns |
| --- | --- |
| Workflow and Agent Turn coordination | Admission, fencing, deadlines, durable state transitions, retries, mutation reconciliation |
| Agent adapter | Pi protocol, prompt completion semantics, session reopening, History Replay, configuration application, normalized Agent Events |
| Execution backend | Isolated execution, transport, termination, inventory, environment resources, backend-specific diagnostics |
| Workspace/state adapters | Workspace preparation and capture, retained-state access, disposable-data cleanup, storage-specific operations |
| Trusted publication service | Git history validation, branch preconditions, publication, GitHub credentials |
| Composition/configuration layer | Backend selection, credentials, concrete adapter construction, deployment-specific settings |

The execution boundary may consist of several focused Go interfaces.
It must not become a single large interface combining unrelated operations.

### 2.2 Core execution guarantees

A conforming backend must provide:

1. Execution identity tied to the Agent Participant, Agent Session, Agent Turn, and execution epoch.
2. Recovery after an ambiguous creation result.
3. Bidirectional non-TTY process transport with separate stderr.
4. Bounded termination and cleanup.
5. Termination of the execution environment's background work.
6. Discovery and cleanup after orchestrator restart.
7. Retained state independent of disposable Runtime Processes.
8. Isolation between Participants and concurrent Turns.
9. Explicit failure and uncertainty reporting.
10. Backend-owned validation of required execution capabilities.

Cancellation of an API call is not evidence that execution stopped.
Missing exit evidence must remain unknown rather than being converted into a fabricated success, failure cause, or OOM diagnosis.

### 2.3 Location independence

Core coordination must use logical workspace and state references rather than host paths, Docker volume names, or Microsandbox resource IDs.

Guest paths such as `/workspace` may remain stable parts of a Runtime Profile.
They must not imply that the orchestrator can access the same path locally.

Workspace access must support a consistent representation of proposed changes.
A local implementation may use local filesystem and Git operations.
A future remote implementation may transfer artifacts.

Trusted publication and its credentials remain local to Omnigrex.

The consistency contract must cover both:

- the committed changes selected for publication;
- the working-tree evidence needed by existing publication and outcome checks.

Do not replace authoritative checks with an unverified agent claim that a workspace is clean.

### 2.4 Durable bindings

Separate the identities of:

- the agent runtime and its session format;
- the execution backend and its configuration contract;
- the repository development image;
- retained state and its owner.

Retain an immutable Runtime Profile binding over the execution-relevant configuration.
Backend-specific storage metadata may be opaque to core code, but must be versioned and validated by its owner.

Repository image changes must not silently alter the execution configuration of retained Participants.
Their selection and upgrade policy is defined in [decision 0009](../decisions/0009-pin-participant-images-and-use-working-revision-tools.md).

### 2.5 Agent boundary

Expose Omnigrex operations rather than Pi RPC messages:

- prepare or reopen an exact session;
- apply and verify requested configuration;
- replay history;
- submit a prompt;
- observe normalized activity and terminal outcomes;
- cancel and cleanly detach.

Keep Pi command names, event framing, session-file parsing, and extension implementation inside the adapter.

Do not promise conversation portability to unrelated future agents.

### 2.6 Instructions and repository skills

Preserve the newer accepted instruction decisions:

- Current-deployment platform and operator instructions apply on each Turn.
- Stage objectives remain distinct from repository preferences.
- Repository skills come from the prepared checkout.
- Repository Agent Profile provenance remains separately controlled.

Reconcile the older Pi specification's blanket skill-disabling language with these requirements.
Allow explicitly authorized repository skills while disabling uncontrolled project extensions, settings, packages, and automatic resource discovery.

## 3. Delivery rules

Each numbered step is suitable for a separate Work Item.
Execute them in order.

Every Work Item must include:

- its prerequisite step;
- its implementation scope;
- its observable acceptance tests;
- the evidence required before starting the next step;
- any temporary compatibility code it introduces and where it will be removed.

For every code change, run the narrow relevant tests and `mise run check`.
Run the applicable integration suite when execution, storage, Docker, or deployment behavior changes.

Introduce backend-specific test entry points as the implementations become available.
Keep destructive Compose tests isolated to environments whose test resources may be removed.

Do not weaken checks or introduce broad dead-code exemptions to accommodate unfinished integration.
New behavior should be reachable through a deliberate application, diagnostic, or qualification path.

## 4. Sequential implementation steps

### Step 01: Prove the Linux Microsandbox deployment and development environment

**Goal:** Establish that the target execution platform can meet the practical requirements.

**Scope**

- Pin a candidate Microsandbox release and SDK.
- Test launching it from the intended Linux deployment arrangement.
- Verify KVM access, process ownership, persistent storage, networking, and resource limits.
- Run Docker, Compose, and a representative Testcontainers workload inside the microVM.
- Run relevant existing Omnigrex Docker-backed integration tests inside it.
- Investigate nested microVM execution and document the result.
- Identify a suitable KVM-capable CI environment.

**Completion gate**

A reproducible experiment demonstrates the intended host-to-microVM-to-Docker topology without exposing the host Docker socket to the guest.

Record exact versions, commands, results, and unresolved limitations.
If required behavior fails, revisit the backend choice or deployment arrangement before production refactoring.

### Step 02: Prove Pi's critical protocol and session behavior

**Goal:** Establish the agent semantics before designing the final adapter.

**Scope**

- Pin Pi and Node versions.
- Exercise non-TTY RPC in Docker and the Microsandbox prototype.
- Verify request correlation, streaming, prompt admission, terminal completion, errors, aborts, and cancellation.
- Prove exact session reopening in a fresh Runtime Process.
- Prove History Replay without submitting a new model prompt.
- Exercise an authenticated MCP extension prototype.
- Test model and thinking configuration without fallback.
- Qualify OpenCode Go and one representative OpenAI API-key model.
- Inspect discovery, instructions, skills, credential paths, and session persistence.

**Completion gate**

Demonstrate continuation across fresh environments, distinguish admission from completion, and recover the intended session after an ambiguous creation result.

Record the actual behavior of the pinned Pi version rather than relying on assumed event names.

### Step 03: Define contracts and record decisions

**Deliverable:** [Execution and agent boundary contracts](2026-10-07-runtime-boundary-contracts.md), including the current-integration inventory, conformance matrix, remote-backend walkthrough, and links to decisions 0005-0009.

**Goal:** Specify the smallest boundary supported by both existing behavior and the experiments.

**Scope**

- Define execution, inventory/recovery, workspace access, and retained-state interfaces.
- Define common identities, errors, exit evidence, and backend capabilities.
- Define creation ambiguity, timeout, and cleanup semantics.
- Define workspace capture and publication consistency.
- Define immutable image selection and retained-Participant upgrade behavior.
- Specify separation between the agent adapter and execution backend.
- Record decisions under `docs/decisions/`, linking applicable superseded ADRs.
- Design the shared conformance suite and architecture dependency checks.

**Completion gate**

Map every current Docker integration point to an owner behind the proposed boundary.
Walk through a hypothetical remote implementation without adding host filesystem assumptions to core code.

Keep any unresolved choice explicitly proposed rather than claiming agreement.

### Step 04: Adapt Docker to the neutral execution and recovery contracts

**Deliverable:** [Docker execution boundary implementation](2026-10-07-docker-execution-boundary.md), including creation reservations, diagnostic identities, conformance evidence, and transitional ownership.

**Goal:** Obtain the first real implementation without changing agent behavior.

**Scope**

- Move Docker SDK types, policies, labels, and container operations behind the adapter.
- Update launch, stop workers, startup inventory, and malformed-resource cleanup.
- Replace core `ContainerID` assumptions with neutral execution references.
- Normalize exit evidence and preserve unknown outcomes.
- Keep the existing OpenCode path operational.

**Completion gate**

Existing Docker behavior passes through the new interfaces.
Test normal execution, creation ambiguity, restart discovery, exact-epoch cleanup, duplicates, and cleanup failure.

Core coordination packages no longer import the Docker implementation or SDK.

### Step 05: Make workspace access and publication location-independent

**Deliverable:** [Location-independent workspace evidence](2026-10-08-location-independent-workspaces.md), including both-backend mid-prompt capture proofs, the versioned artifact contract, and retained terminal observations.

**Goal:** Remove the requirement that core code can directly inspect the live agent checkout.

**Scope**

- Begin with a focused consistent, mid-prompt workspace-capture experiment on both backends before implementing the capture artifact format.
- Introduce logical workspace references and defined access/capture operations.
- Adapt preparation, publication, outcome reconciliation, terminal corroboration, and discard.
- Preserve multi-commit history validation and expected-remote-head checks.
- Keep trusted publication and GitHub credentials outside execution.
- Implement efficient local workspace access.
- Prove consistent capture during an outstanding MCP call without waiting for that call's prompt to finish.
- Take a fresh terminal capture after confirmed stop, and retain it until settlement or deferred corroboration no longer needs it.

**Completion gate**

Publication and outcome validation succeed using a separately materialized workspace representation without access to the original live checkout.

Regression tests cover dirty workspaces, invalid history, changed remote heads, concurrent workspace changes, and uncertain publication responses.
An adapter unable to establish a consistent capture must fail rather than publish from a best-effort directory copy.

### Step 06: Abstract retained state and bind it durably to its backend

**Deliverable:** [Retained-state implementation and evidence](2026-10-08-retained-state-bindings.md), including the forward-only Docker transition, immutable Backend ownership, protected-state availability, archive validation, and fresh-environment Session Continuation after restore.

**Goal:** Preserve Session Continuation and recovery without storage-specific assumptions.

**Scope**

- Introduce logical retained-state references.
- Adapt retention, deletion, availability, backup/export, and restore integration.
- Persist backend ownership and versioned state metadata.
- Add forward-only schema changes with a transitional path for existing Docker operation.
- Keep disposable credentials and scratch data outside retained state.
- Reject mismatched backend/state bindings.

**Completion gate**

Destroy and recreate an execution environment while preserving only intended retained state.
Prove cross-Participant isolation, idempotent cleanup, protected-state retention, and explicit rejection of backend mismatch.

### Step 07: Introduce the repository environment contract and in-environment tooling

**Deliverable:** [Repository environment implementation and evidence](2026-10-08-repository-environments.md), including strict image selection, immutable contract persistence, golden-resource qualification, and working-revision tool installation for both Roles.

**Goal:** Support reproducible repository environments without orchestrator-side tool installation.

**Scope**

- Define and implement digest-pinned repository image configuration.
- Establish the golden-image contract and build examples.
- Verify required runtime resources rather than treating image ancestry as proof.
- Move repository-defined tool installation inside execution environments.
- Use the working revision for both Roles.
- Preserve default-branch Agent Profile provenance separately.
- Keep runtime state, workspace data, and tool caches distinct.
- Validate requested backend capabilities before starting work.

**Completion gate**

A repository image supplies an additional development tool.
Both Roles use the correct revision's tool configuration.
A repository installation script executes inside the environment rather than the orchestrator.

A request for unsupported capabilities fails clearly on Docker.

### Step 08: Implement the Microsandbox backend

**Deliverable:** [Microsandbox backend implementation and qualification](2026-10-09-microsandbox-backend.md), including the pinned Go SDK, selected-backend composition, durable dispatch barriers, POSIX-faithful capture/state bindings, and Linux/KVM evidence.

**Goal:** Add the second implementation without changing core coordination.

**Scope**

- Implement the agreed lifecycle, transport, inventory, recovery, workspace, and state contracts.
- Add Linux/KVM configuration and preflight integration.
- Implement environment-wide termination, including nested Docker workloads.
- Provide persistent session storage and disposable credential/configuration storage.
- Integrate required MCP connectivity.
- Preserve execution identities across API failures and orchestrator restarts.

**Completion gate**

Run the same core conformance tests against Docker and Microsandbox.

A disposable protocol test process behaves equivalently on both.
Microsandbox additionally passes Docker/Compose/Testcontainers qualification.

Adding this implementation requires no backend branches in Workflow or Agent Turn policy.

### Step 09: Implement the Pi protocol adapter

**Deliverable:** [Pi protocol adapter implementation and evidence](2026-10-09-pi-protocol-adapter.md), including credential-safe operational events and the shared controlled-provider diagnostic suite.

**Goal:** Replace protocol coupling with tested Pi-specific mechanics.

**Scope**

- Implement correlated JSONL communication and continuous event reading.
- Distinguish prompt admission from completion.
- Normalize provider failures, aborts, malformed responses, and process loss.
- Implement bounded cancellation and cleanup.
- Preserve useful runtime-specific diagnostics without exposing credentials.
- Integrate a diagnostic path while retaining the existing default runtime during preparation.

**Completion gate**

Controlled-provider tests cover success, failure after admission, lost terminal messages, timeouts, cancellation, and process exit on both backends.

No core Workflow code consumes Pi RPC structures.

### Step 10: Implement Pi configuration, MCP integration, and instruction handling

**Deliverable:** [Pi configuration and MCP implementation and evidence](2026-10-09-pi-configuration-and-mcp.md), including the candidate Profile schema, explicit resource ownership, and shared both-backend authority tests.

**Goal:** Preserve the platform's capabilities and instruction contracts with Pi.

**Scope**

- Package the Omnigrex-owned MCP extension.
- Use the exact per-Turn authenticated endpoint and tool schemas.
- Translate local-tool permissions to Pi's vocabulary.
- Support explicit `provider/model` and optional thinking configuration.
- Apply current platform/operator instructions and Stage objectives.
- Support explicitly authorized checkout-local repository skills.
- Disable uncontrolled runtime-resource discovery.
- Supply ephemeral Pi-native API-key configuration on both backends.
- Remove OpenCode-specific configuration concepts from the new Profile schema.

**Completion gate**

A Developer can invoke authorized capabilities.
A Reviewer cannot invoke Developer-only MCP tools or disallowed Pi edit/write tools.

Poisoned project configuration cannot load arbitrary extensions or override runtime settings.
Retained sessions receive updated deployment instructions on the next Turn.

Unsupported models or thinking values fail without fallback.

### Step 11: Implement durable Pi Agent Sessions and History Replay

**Deliverable:** [Durable Pi Session implementation and evidence](2026-10-09-durable-pi-sessions.md), including exact pre-prompt identity, runtime-specific schema fields, fenced coordination, and both-backend continuation/replay qualification.

**Goal:** Integrate Pi persistence with Omnigrex's durable ownership model.

**Scope**

- Bind deterministic Pi session identity to the durable Agent Session.
- Add runtime-appropriate identity/capability schema fields.
- Preserve fenced session preparation and prompt admission.
- Implement exact-session recovery after interrupted creation.
- Reopen sessions across fresh environments and replaced workspaces.
- Translate persisted history into Agent Events.
- Preserve human-controlled prompt admission and replay requirements.
- Keep provider retries bounded by the Agent Turn deadline.

**Completion gate**

Both backends pass continuation and replay tests.
History Replay creates no new model activity.
Lost creation responses cannot bind the wrong session.

Concurrent Turns cannot share session state, credentials, or authority.

### Step 12: Connect Pi to complete Agent Turns on both backends

**Deliverable:** [Complete Pi Agent Turn implementation and evidence](2026-10-09-complete-pi-agent-turns.md), including candidate configuration, shared launch/settlement ownership, and controlled both-backend Workflows.

**Goal:** Prove the full vertical integration.

**Scope**

- Wire backend selection, Pi preparation, prompting, MCP authority, and cleanup.
- Preserve existing outcome reconciliation and terminal-intent semantics.
- Run a controlled Developer-to-Reviewer Workflow separately on each backend.
- Keep GitHub mutation authority and publication behavior unchanged.
- Expose the candidate configuration for qualification before deployment cutover.

**Completion gate**

The same controlled Work Item reaches human review using Pi on Docker and Pi on Microsandbox.

Backend selection changes execution infrastructure, not Workflow decisions.

### Step 13: Qualify failure recovery and boundary independence

**Deliverable:** [Pi failure recovery and boundary independence](2026-10-10-pi-failure-recovery.md), including the shared fault matrix, durable replacement barriers, mutation reconciliation, and dependency checks.

**Goal:** Prove the architecture beyond happy-path execution.

**Scope**

Exercise:

- orchestrator restart during creation and prompting;
- lost transport after prompt admission;
- runtime exit and resource exhaustion;
- lease expiry and stale execution;
- MCP token expiry;
- terminal mutation success with a lost response;
- cleanup failure;
- concurrent Turns;
- workspace capture interrupted during publication.

Add import/dependency checks preventing implementation-specific types from leaking into core packages.

**Completion gate**

Both backends satisfy the same recovery invariants.
Uncertain execution is not treated as stopped.
A prior execution cannot overlap a replacement that assumes exclusive access.
External mutations remain reconciled rather than blindly repeated.

### Step 14: Qualify Pi upgrades and state recovery

**Deliverable:** [Pi upgrades and state recovery](2026-10-10-pi-upgrades-and-state-recovery.md), including backend-scoped exact qualification artifacts, copy-based upgrade tests, portable Session archives, and retained-binding protection.

**Goal:** Preserve retained Agent Sessions after the initial clean cutover.

**Scope**

- Adapt exact source-to-candidate qualification artifacts for Pi.
- Bind results to relevant runtime, image, platform, state-format, and backend contract identities.
- Test Pi-to-Pi continuation and replay on both backends.
- Run candidate-image tests on isolated copies; preserve retained production bindings and source state.
- Qualify backup and restore to another compatible host.
- Verify protected image/state availability and retention.
- Reject unsupported image or backend changes.

**Completion gate**

A source-image Pi session continues and replays under a qualified candidate.
An unqualified change is rejected.
Restored state remains bound to the correct Participant and backend.

### Step 15: Complete operational integration and CI

**Deliverable:** [Pi operational integration and backend CI](2026-10-11-pi-operational-integration.md), including backend-selected diagnostics, image-only deployment qualification, required KVM gates, and the remaining merged-release publication boundary.

**Goal:** Make both supported backends deployable and diagnosable.

**Scope**

- Finish backend-selected doctor and readiness behavior.
- Publish pinned Pi/golden images and deployment artifacts.
- Add the Linux/KVM Microsandbox deployment configuration.
- Keep Docker deployment free of unnecessary Microsandbox host requirements.
- Add explicit Docker and Microsandbox integration entry points.
- Run shared conformance and Pi lifecycle suites on both.
- Run microVM tests on KVM-capable CI.
- Validate image-only, no-clone deployments.

**Completion gate**

Each deployment passes its applicable diagnostics and smoke tests.
Missing KVM or unsupported capabilities fail before accepting work.
Diagnostic sessions leave no retained runtime state.

### Step 16: Complete operator documentation and cutover rehearsal

**Goal:** Make the final transition reproducible.

**Scope**

- Update `README.md`, `docs/operator-guide.md`, domain terminology where necessary, and decision records.
- Document backend guarantees and selection.
- Document repository image construction and qualification.
- Document credential placement, backup scope, restore, and retained-image requirements.
- Rehearse the one-time OpenCode reset and Pi deployment in an isolated environment.
- Document rollback limits after beginning fresh Pi work.
- Reconcile the original GitHub and ClickUp requirements with the delivered scope.

**Completion gate**

An operator can deploy, diagnose, back up, restore, and rehearse cutover using the documentation and published artifacts.

Keep operational procedures authoritative in the operator guide.

### Step 17: Remove OpenCode and perform final qualification

**Goal:** Finish with Pi-only agent support and two retained execution backends.

**Scope**

- Remove OpenCode/ACP implementation, images, configuration, transitional paths, and obsolete tests.
- Retain Docker and Microsandbox implementations and qualification suites.
- Run final checks and both backend integration gates.
- Run destructive deployment tests only in disposable environments.
- Complete an isolated dogfooding Work Item using Microsandbox.
- Perform the operator-controlled clean cutover when the release is ready.

**Completion gate**

The final code and deployment artifacts contain no active OpenCode runtime path.
Pi works on both backends.
Microsandbox Agent Turns can run the agreed Docker-backed development tests.
The released deployment passes the documented clean-cutover and recovery gates.

## 5. Verification matrix

| Behavior | Docker | Microsandbox |
| --- | --- | --- |
| Lifecycle, transport, restart recovery | Required | Required |
| Workspace capture and trusted publication | Required | Required |
| Pi continuation and History Replay | Required | Required |
| Fenced MCP authority and outcome reconciliation | Required | Required |
| Ephemeral provider credentials | Required | Required |
| Working-revision repository tooling | Required | Required |
| Prebuilt repository image contract | Required | Required |
| Qualified Pi upgrades and restore | Required | Required |
| Stronger microVM host isolation | Not claimed | Required |
| Docker/Compose/Testcontainers inside an Agent Turn | Not required | Required |
| Nested Microsandbox integration tests inside an Agent Turn | Not required | Investigate only |

Failure of a backend-specific capability test must not be hidden by weakening shared lifecycle guarantees.

## 6. Explicit exclusions

- A production remote backend.
- Automatic cross-backend retained-state migration.
- OpenCode conversation conversion.
- Permanent multi-agent-runtime support.
- Native macOS Microsandbox deployment.
- Automatic repository image builds.
- Provider credential mediation or a new provider proxy.
- Provider OAuth/subscription authentication.
- A model/provider catalog.
- A human session UI.
- An OpenCode-style step limiter.
- Full Docker/Microsandbox security or nested-container feature parity.
- Changes to the accepted `docs/plans/first-iteration.md` specification.

## 7. Final architectural acceptance criterion

A future remote backend may require new execution, storage, transfer, networking, and deployment implementations.

It must not require changes to:

- Workflow transitions;
- Agent Turn admission and settlement policy;
- Agent Session control ownership;
- MCP authorization;
- Change Proposal publication policy;
- the Pi protocol implementation.

This claim is conditional on the new backend satisfying the documented contract.
Provider incompatibilities must be explicit rather than concealed through silent degradation.
