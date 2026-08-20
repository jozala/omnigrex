# First-Iteration Implementation Plan

This plan implements the behavior defined in [First Iteration](./first-iteration.md).
Each phase has a verification gate and should leave the repository in a working state.

## Engineering Principles

- Keep workflow decisions deterministic and outside model output parsing.
- Keep ACP and OpenCode details behind the runtime boundary.
- Keep GitHub credentials outside agent Runtime Processes.
- Use durable jobs and idempotent transitions before adding concurrency.
- Build one vertical path before generalizing any adapter or workflow definition.
- Persist only the operational data required by the first iteration.
- Use current GitHub state to reconcile partial external effects.
- Pin every runtime and infrastructure dependency used to restore an Agent Session.

## Target Repository Structure

The exact package boundaries may evolve, but the initial structure should follow these responsibilities:

```text
cmd/omnigrex/                 process entry point
internal/config/              deployment and repository configuration
internal/workflow/            domain state and transition rules
internal/store/               PostgreSQL repositories and migrations
internal/github/              GitHub App clients, webhooks, and mutations
internal/jobs/                durable queue, leasing, and workers
internal/runtime/acp/         ACP v1 client and runtime-neutral session port
internal/runtime/docker/      Runtime Process lifecycle and mounts
internal/mcp/                 scoped Omnigrex Tool Gateway
internal/workspace/           clone, refresh, snapshot, commit, and push
migrations/                   embedded SQL migrations
agent/opencode/               common non-root OpenCode image
config/runtime-profiles/      immutable deployment Runtime Profiles
.omnigrex/team/               dogfooding Agent Profiles
compose.yaml                  development and deployment stack
mise.toml                     pinned toolchain and development tasks
```

The Go module path is `github.com/jozala/omnigrex`.
Go 1.27 is pinned through mise and the build image.

## Phase 1: Foundation

### Work

- Initialize the Go module and pin direct dependencies.
- Add mise configuration for Go, formatting, linting, tests, and Docker Compose validation.
- Add a minimal `cmd/omnigrex` process with structured logging and graceful shutdown.
- Add unit-test and integration-test package conventions.
- Add `.gitignore`, `.dockerignore`, and environment examples without secret values.
- Add a continuous integration workflow for formatting, linting, unit tests, and image builds.
- Embed SQL migrations in the Go binary, but defer domain tables until their phases.

### Gate

- `mise install` provisions the development toolchain.
- `mise run check` passes from a clean checkout.
- The Go binary starts and shuts down cleanly.
- The initial image runs with a non-zero user ID.

## Phase 2: ACP Compatibility Spike

This phase must pass before webhook or workflow implementation begins.

### Minimal ACP Client

- Implement ACP v1 JSON-RPC framing over standard input and output.
- Support request correlation, notifications, agent-to-client requests, cancellation, deadlines, and connection failure.
- Implement only `initialize`, `session/new`, `session/list`, `session/resume`, `session/load`, `session/set_config_option`, `session/prompt`, and `session/cancel` initially.
- Handle `session/update` and `session/request_permission` without persisting content.
- Answer autonomous permission requests from the Agent Profile and deny unknown requests.
- Advertise no ACP client filesystem or terminal capabilities in the first iteration.
- Preserve unknown update fields so future Agent Event normalization does not require changing the transport layer.
- Expose runtime-neutral `CreateSession`, `ContinueSession`, `ReplayHistory`, `Prompt`, and `CancelTurn` operations.
- Model Session Continuation and History Replay as separate negotiated capabilities.

### OpenCode Runtime Process

- Build a pinned, non-root OpenCode image that runs `opencode acp` with automatic updates and session sharing disabled.
- Include Git, shell utilities, certificates, and a pinned mise binary.
- Mount a disposable workspace at exactly `/workspace`.
- Identify and document the exact OpenCode/XDG state paths, database files, sidecar files, and environment variables required for Session Continuation.
- Mount those OpenCode runtime-state paths from a separate persistent assignment directory.
- Keep provider credentials out of the persistent runtime-state directory.
- Run Reviewer sessions in pure mode with feature-branch project plugins, MCP servers, agents, and instruction discovery disabled while preserving those files for direct review.
- Attach ACP through the Docker Engine API and demultiplex standard error from protocol output.
- Label every Runtime Process with assignment, session, turn, and Runtime Profile identifiers.

### Same-version Continuation Test

1. Start pinned OpenCode version A in a fresh Runtime Process.
2. Negotiate ACP v1 and record the capability snapshot.
3. Create an Agent Session at `/workspace`.
4. Send a prompt containing a unique fact and exercise a test MCP tool.
5. Stop and remove the Runtime Process.
6. Replace the workspace contents while mounting the new workspace at the same `/workspace` path.
7. Start a fresh Runtime Process with version A and the original runtime-state directory.
8. Continue the Agent Session and verify that the unique fact remains in context.
9. Replay history and verify that the prior interaction is emitted.
10. Attempt continuation with another working directory and verify that Omnigrex rejects it before sending the ACP request.
11. Lose the `session/new` response after OpenCode persists state and recover the only session through `session/list` without creating another one.
12. Kill OpenCode during provider streaming, a local tool call, an MCP call, and after an MCP side effect but before tool-result delivery; continue the same session and accept another prompt in every case.

### Controlled Upgrade Test

1. Create and use an Agent Session with pinned OpenCode version A.
2. Stop the Runtime Process and copy its runtime-state directory.
3. Start pinned candidate version B with the copied state and the same `/workspace` path.
4. Continue the Agent Session and send a prompt that depends on prior context.
5. Exercise the test MCP tool and replay session history.
6. Compare capability snapshots and role configuration behavior between versions.
7. Record compatibility against the exact source image digest, target image digest, platform, and runtime-state contract.
8. Preserve the original copy so a failed or destructive migration is recoverable.
9. Create a separate new session under version B, remove its Runtime Process, and prove B-to-B continuation, History Replay, and ambiguous-creation recovery.

The test accepts image references as parameters so the previous stable and candidate versions can be tested before a Runtime Profile upgrade.
The first iteration does not migrate existing sessions even after a pair passes; the result preserves a future migration option and qualifies the candidate for new Assignments.

### Gate

- A fresh container can continue a session created by a removed container.
- History Replay works independently of the continuation path.
- The session fails fast when `/workspace` is not stable.
- Cancellation returns a terminal turn result.
- Two concurrent prompts for one Agent Session are rejected.
- Loss of the initial session response recovers one session rather than creating a duplicate.
- The exact previous-stable to candidate version pair has a documented compatibility result.
- The candidate passes its own same-version create, dispose, continue, replay, and creation-recovery gate.
- Feature-branch OpenCode configuration cannot add Reviewer instructions, agents, plugins, MCP servers, or capabilities.

## Phase 3: Docker Compose and PostgreSQL

### Work

- Add `compose.yaml` with orchestrator and PostgreSQL services plus a one-shot OpenCode image validation service that is built during normal startup.
- Run every service process as a fixed non-root UID and GID.
- Mount the Docker socket only into the orchestrator and add its numeric group through deployment configuration.
- Create separate named volumes for PostgreSQL, workspaces, and runtime session state.
- Create separate backend and agent networks so Runtime Processes cannot reach PostgreSQL.
- Expose only the orchestrator HTTP listener to the host.
- Read GitHub App keys, webhook secrets, database credentials, and provider credentials from Compose secrets.
- Require Docker Engine API 1.45 or newer and stable explicit names for the volumes and agent network used by dynamically created Runtime Processes.
- Create and ownership-check assignment volume subdirectories before mounting them through Docker volume subpaths.
- Configure dropped capabilities, `no-new-privileges`, process and memory limits, a read-only root filesystem, and bounded temporary filesystems for Runtime Processes.
- Define writable HOME, XDG, cache, state, workspace, and per-assignment mise paths explicitly.
- Add liveness and readiness endpoints.
- Refuse readiness when the database, Docker API, required API version, network, volumes, or agent image is unavailable.
- Make readiness perform a non-root create, subpath mount, write, and cleanup probe against the workspace and runtime-state volumes.

### Initial Schema

- `webhook_deliveries`
- `workflows`
- `workflow_attempts`
- `agent_assignments`
- `agent_sessions`
- `agent_turns`
- `change_proposals`
- `tool_invocations`
- `jobs`

Identifiers use GitHub repository and Issue database IDs rather than names and numbers when durable identity is required.
Names and numbers remain denormalized for display and API calls.

### Gate

- `docker compose up` reaches healthy status with configured secrets.
- PostgreSQL migrations run exactly once and are safe under concurrent startup.
- Restarting the orchestrator preserves database, runtime-state, and workspace volumes.
- No process in the stack runs with UID 0.
- Runtime Processes cannot connect to the PostgreSQL network.
- Normal `docker compose up --build` builds and validates the agent image on a clean host.
- A Runtime Process can use every declared writable path while its root filesystem remains read-only.
- Runtime Processes have no added Linux capabilities and enforce the configured process and memory limits.

## Phase 4: Durable Webhooks and GitHub Apps

### Work

- Validate `X-Hub-Signature-256` before parsing a webhook.
- Insert the raw delivery, essential headers, processing status, and claim metadata before responding with `202 Accepted`.
- Deduplicate by the globally unique GitHub delivery identifier.
- Claim unprocessed inbox rows with expiring leases so acknowledged deliveries cannot be stranded by a crash.
- Parse only the Issue, Pull Request, and Pull Request Review actions used by the first iteration.
- Ignore unsupported events after recording their delivery result.
- Implement Developer/Orchestrator App JWT authentication and installation-token caching.
- Resolve the Reviewer App installation for the same repository independently.
- Add GitHub API clients behind testable interfaces.
- Create and reconcile the five Omnigrex labels.
- Add hidden Workflow and Assignment markers to generated GitHub artifacts.

### Required App Permissions

The Developer/Orchestrator App requires repository metadata and Checks read access and read/write access to contents, Issues, Pull Requests, and Workflows.
It subscribes to Issue, Pull Request, and Pull Request Review events.

The Reviewer App requires repository metadata and contents read access plus Pull Request write access.
Its GitHub App webhook is disabled rather than merely left without selected repository events.

### Gate

- Invalid signatures are rejected without database writes.
- Duplicate valid deliveries produce one queued transition.
- Crashes after inbox insertion, normalization, state mutation, and job creation cannot lose or duplicate a transition.
- Installation-token refresh is safe under concurrency.
- Missing Reviewer installation or permission produces a Human Handoff instead of an unbounded retry.
- Mock GitHub tests cover pagination, rate limits, transient failures, and permission failures.
- Live tests prove the Reviewer App can submit both `APPROVE` and `REQUEST_CHANGES` reviews on a Developer App Pull Request.

## Phase 5: Workflow State Machine and Durable Jobs

### Work

- Implement workflow transitions as pure functions over current state and a normalized event.
- Keep inbox completion, normalized-event identity, state mutation, and job creation in one PostgreSQL transaction.
- Lease jobs with `FOR UPDATE SKIP LOCKED`, lease expiry, bounded attempts, and heartbeats.
- Allocate a monotonically increasing execution epoch for every Agent Turn.
- Include the epoch in job ownership, Runtime Process labels, MCP tokens, mutations, and turn finalization.
- Validate the current epoch transactionally before each side effect and terminate Runtime Processes carrying stale epochs.
- Before granting a newer epoch, close prior mutation admission, stop the prior Runtime Process, and settle or escalate every admitted invocation.
- Enforce one active Workflow Attempt per Issue.
- Enforce one active Agent Turn per Agent Session.
- Enforce the deployment-wide Agent Turn concurrency limit, defaulting to two.
- Consume `omnigrex:run` only after the Workflow Attempt is durably created.
- Persist pending GitHub events that arrive while an Agent Turn is active.
- Reconcile pending events when that turn ends and enqueue at most one successor.

### Transition Tests

- New Issue trigger to initial Developer Turn.
- Developer Pull Request to Reviewer Turn.
- Change request to returning Developer Turn.
- Approval to `omnigrex:pr-ready`.
- Third successfully published change-requesting Review Cycle to `omnigrex:needs-human`.
- Stale review before acceptance does not consume a Review Cycle.
- Synchronization after the third accepted review invalidates readiness and creates a Human Handoff rather than a fourth Review Cycle.
- Agent blocker to `omnigrex:needs-human`.
- One infrastructure retry without consuming review budget.
- Second infrastructure failure to Human Handoff.
- Re-added trigger to a new Workflow Attempt with reset budgets, reused sessions, and the existing open Change Proposal.
- Issue closure fences active work, completes Assignments, and schedules runtime-state retention.
- Issue reopening cancels garbage collection without starting automation.
- A new trigger after reopening reactivates retained Assignments, while a trigger after garbage collection creates new ones.
- Out-of-order, duplicate, stale, and unrelated webhooks.

### Gate

- The transition test suite exhaustively rejects illegal state changes.
- Killing a worker after leasing a job causes safe recovery after lease expiry.
- A stale worker cannot start a Runtime Process, call a mutation tool, or finalize its Agent Turn after another epoch takes ownership.
- No webhook ordering can create concurrent turns for one Agent Session.

## Phase 6: Profiles, Assignments, Sessions, and Turns

### Work

- Define and validate immutable deployment Runtime Profiles.
- Add the initial `opencode-acp/v1` profile with exact image digest, ACP command, environment, persistent state mounts, required capabilities, supported platform, writable paths, and `/workspace`.
- Parse `.omnigrex/team/developer.md` and `reviewer.md` from the latest default branch.
- Validate YAML front matter, Role instructions, runtime reference, model, variant, steps, and permissions.
- Persist the Agent Profile commit SHA and content hash used for every Agent Turn.
- Bind an Assignment to its initial Runtime Profile version and reject a later profile runtime-reference change with a configuration Human Handoff.
- Allow updated instructions, model, variant, steps, and permissions to apply without changing the Agent Session identity.
- Render Role instructions, permissions, and process-level limits into a read-only OpenCode configuration owned by the runtime adapter and selected for the ACP session.
- Ensure runtime-owned safety configuration has precedence over repository OpenCode configuration.
- Create one Developer Assignment and one Reviewer Assignment per Issue.
- Create an Agent Session lazily on an Assignment's first turn.
- Persist the ACP session identifier immediately after `session/new` succeeds.
- Recover an ambiguous first session creation through assignment-isolated `session/list` before retrying `session/new`.
- Apply supported model, variant, and mode values through `session/set_config_option` after every create or continuation operation.
- Apply steps and permissions through the Runtime Profile's process-level OpenCode configuration and verify the effective configuration.
- Reuse the same Agent Session for retries, review cycles, and Workflow Attempts.
- Represent every retry as a new Agent Turn linked to the failed or interrupted turn rather than as an attempt nested inside one turn.
- Persist only normalized operational metadata from ACP updates in the first iteration.
- Emit runtime-neutral Agent Events to an in-process no-op sink so persistence can be added without changing the ACP client.

### Control

- Persist Assignment status independently of Control Owner.
- Store a monotonically increasing control revision on every Agent Session.
- Require a matching control revision to start an Agent Turn.
- Implement only the `AUTOMATION` Control Owner in product behavior.
- Keep internal acquire, release, and cancellation operations suitable for a later human controller.
- Persist future controller identity and acquisition metadata even though no human-facing endpoint is exposed.

### Gate

- Initial and returning turns use the same session identifier.
- Developer and Reviewer sessions cannot see each other's runtime state.
- A stale control revision cannot submit a prompt.
- Mutable Agent Profile changes on the default branch apply to the next turn without changing session identity.
- A Runtime Profile reference change cannot mutate an existing Assignment.
- The effective OpenCode model, variant, steps, and permissions match the latest permitted Agent Profile values.
- A Pull Request that modifies `.opencode`, project MCP configuration, plugins, agents, or instruction files cannot change the Reviewer's effective configuration.
- A fake human controller integration test fences automation, waits for or cancels the active turn, drains MCP mutations, replays history, submits a prompt, and returns control without dual prompts.

## Phase 7: Workspaces and MCP Tool Gateway

### Workspace Lifecycle

- Create an isolated assignment workspace subdirectory in the orchestrator-mounted named volume.
- Mount only that subdirectory at `/workspace` in a Runtime Process.
- Clone and refresh through the orchestrator using short-lived Developer App tokens.
- Keep credentials out of Git remotes and the agent environment.
- Check out the default branch for initial development and the Pull Request head for subsequent turns.
- Install and activate repository mise tools into an assignment-isolated data directory before starting the Agent Turn when mise configuration is present.
- Use the Developer workspace mise revision for Developer Turns and the latest default-branch mise configuration and lock data for Reviewer Turns.
- Keep feature-branch mise files visible for review without evaluating or executing them.
- Prepare publication in a clean checkout that the agent cannot modify.
- Copy the agent's file tree without its Git metadata into the clean checkout.
- Disable Git hooks and validate the expected base and branch before every commit and push.
- Compare normalized workspace and published trees, including deletions, executable bits, and symlinks, instead of relying on the agent checkout's Git index.

### MCP Transport and Authorization

- Implement streamable HTTP MCP on the private agent network.
- Pass its configuration through ACP session creation and continuation.
- Issue a random per-turn token scoped to Assignment, Session, execution epoch, repository, Issue, Pull Request, and allowed tools.
- Reject expired tokens, stale epochs, and tokens used outside their active Agent Turn.
- Serialize mutation tools per Agent Session.
- Reserve a stable operation identifier before every external mutation and persist `RESERVED`, `IN_FLIGHT`, `UNKNOWN`, `SUCCEEDED`, or `FAILED` state.
- Embed the operation identifier in reconcilable GitHub artifacts and cache successful results.
- Use expected old heads and deterministic commit data for Git pushes.
- Close mutation admission when the ACP prompt ends, drain all admitted calls, reconcile `UNKNOWN` outcomes, and revoke the token before finalizing the Agent Turn.
- Move unresolved `UNKNOWN` outcomes to durable `RECONCILING` work, release the Runtime Process and concurrency slot, and keep successor scheduling blocked.
- Bound reconciliation retries and create a Human Handoff when the external result remains unknowable.
- Record request, result metadata, duration, and idempotency identity without storing model reasoning.

### Read Tools

- Implement `get_issue`.
- Implement `list_issue_comments`.
- Implement `get_pull_request`.
- Implement `list_pull_request_reviews`.
- Implement `list_review_threads`.
- Implement `get_check_runs`.

### Mutation Tools

- Implement `publish_changes` using the clean publication checkout.
- Implement `open_pr` with Issue linkage and hidden Workflow markers.
- Implement `request_review` as an internal durable handoff without requiring a new commit or GitHub requested-reviewer assignment.
- Implement `submit_review` under the Reviewer App identity, bind it to an expected head and active Assignment, and validate inline comment locations.
- Persist the submitted review ID, Reviewer App actor identity, and reviewed commit ID before accepting its webhook as a transition.
- Implement `comment_on_issue` and `comment_on_pull_request` with idempotency markers.
- Implement `report_blocked` as an explicit Human Handoff.

### Gate

- Agents can read current GitHub context but cannot obtain GitHub credentials.
- Multiple publish calls create ordered commits without concurrent Git corruption.
- Repeating a mutation after an ambiguous network failure does not create duplicate artifacts.
- Process death immediately after remote success is reconciled from the reserved operation identifier.
- ACP cancellation or MCP connection loss cannot finalize a turn until every admitted mutation is terminal or reconciled.
- Lease takeover cannot admit a newer execution epoch until every prior-epoch mutation is settled or escalated.
- Invalid tool use by the wrong Role or against another repository is rejected.
- Review webhooks from unrelated actors or stale heads cannot advance the Workflow.
- A webhook caused by an active turn cannot start the next turn before the current one completes.
- Repository mise installation succeeds from empty assignment storage without exposing another Assignment's mutable tools.
- Feature-branch mise changes cannot alter the Reviewer's effective commands or installed tool versions.

## Phase 8: Developer and Reviewer Vertical Flow

### Work

- Generate deterministic event envelopes for initial development, requested changes, review, retries, and reactivated Workflow Attempts.
- Send only identifiers, triggering event, current head, expected outcome, and capabilities.
- Do not embed Issue bodies, comments, reviews, checks, or interpreted findings.
- Configure Developer permissions for editing and local commands while denying direct GitHub publication.
- Configure Reviewer permissions without editing or publication tools while allowing local verification commands whose workspace effects are discarded.
- Require successful Developer outcomes to leave no unpublished normalized tree changes.
- Require Reviewer outcomes to come from the active Reviewer App identity and target the expected Pull Request head.
- Reread the Pull Request head during review reconciliation and do not count a review that is already stale.
- Store `ready_for_sha` for an accepted approval and reconcile the advisory `omnigrex:pr-ready` label against it.
- Remove stale readiness on every Pull Request synchronization and create a Human Handoff when another review would exceed the budget.
- Apply and reconcile visible labels after every state transition.
- Post concise diagnostics to GitHub on blockers and terminal failures.

### Gate

- A real Developer Session can implement a small dogfooding Issue and open a normal Pull Request.
- A separate real Reviewer Session can approve it through the Reviewer App.
- A requested change continues the original Developer Session in a fresh Runtime Process.
- Non-blocking findings can accompany approval and `omnigrex:pr-ready`.
- A push racing review submission cannot make a stale approval ready for human review.
- A push after readiness eventually removes `omnigrex:pr-ready` when its head no longer equals `ready_for_sha`.
- The full three-Review-Cycle escalation path ends at `omnigrex:needs-human`.

## Phase 9: Recovery, Retention, and Upgrade Safety

### Work

- Reconcile database turns with Docker-labeled Runtime Processes during startup.
- Never adopt an in-flight ACP prompt after the orchestrator loses its JSON-RPC connection.
- Fence and stop the old Runtime Process, mark the Agent Turn interrupted, drain or reconcile admitted MCP mutations, and retry with a new Agent Turn through a fresh Runtime Process using the same Agent Session.
- Reconcile GitHub state before retrying any turn that may have performed MCP mutations.
- Retry failed or timed-out turns once in the same Agent Session.
- Fence active work, complete Assignments, mark sessions retained, and set `retained_until` when an Issue closes.
- Default retention to 30 days.
- Cancel deletion without restarting automation when the Issue reopens before garbage collection.
- Reactivate retained Assignments only after a new `omnigrex:run` command creates a Workflow Attempt.
- Create new Assignments and Agent Sessions when an Issue is reopened or triggered after its prior state was collected.
- Add an idempotent garbage collector for expired opaque runtime state while retaining PostgreSQL operational history and marking the state deleted.
- Keep Runtime Profile images referenced by active or retained runtime state available until that state is garbage-collected.
- Add the controlled upgrade test to the release checklist.

### Gate

- Restarting the orchestrator during every major transition eventually reaches one valid state.
- Closing an Issue during an active Agent Turn fences new work and settles admitted mutations before completing the Assignments.
- Issue closure preserves session state through the retention deadline.
- Reopening before GC preserves the prior sessions but requires a new trigger before continuation.
- GC removes only expired opaque runtime state without affecting active or retained sessions or deleting PostgreSQL history.
- The first iteration never replaces a session's pinned profile, and every candidate pair still receives a digest-specific compatibility result before use for new Assignments.

## Phase 10: Operator Documentation and Dogfooding

### Work

- Document creation, permissions, event subscriptions, secrets, and installation of both GitHub Apps.
- Document reverse-proxy requirements and webhook URL configuration.
- Document Compose deployment, Docker socket group configuration, backup, restore, and upgrades.
- Document `.omnigrex/team` configuration with Developer and Reviewer examples.
- Document trusted-repository limitations and provider credential exposure.
- Document workflow labels, retry behavior, review budgets, session retention, and manual recovery.
- Add a diagnostic command that validates GitHub Apps, PostgreSQL, Docker, Runtime Profile images, ACP capabilities, and MCP connectivity without starting a Workflow.
- Dogfood Omnigrex on this repository with a deliberately small Issue.

### Gate

- A new operator can deploy the stack using only repository documentation and their secrets.
- The diagnostic command identifies every missing prerequisite before the first Issue is labeled.
- The dogfooding Issue reaches `omnigrex:pr-ready` without GitHub credentials or direct GitHub API access in either agent.

## Test Strategy

### Unit Tests

- State transitions and invariants.
- Role and Runtime Profile parsing.
- ACP message encoding, decoding, correlation, cancellation, and capability mapping.
- Webhook signature validation and normalization.
- MCP authorization and argument validation.
- GitHub label and idempotency reconciliation.
- Retention and control revision rules.

### Integration Tests

- PostgreSQL migrations, transactions, leases, and concurrent workers.
- Docker Runtime Process creation, non-root identity, read-only root filesystem, writable paths, capabilities, networks, subpath mounts, cleanup, and resource limits.
- Fake ACP agent behavior, malformed messages, process crashes, and permission requests.
- Real pinned OpenCode continuation, replay, cancellation, MCP connection, and controlled upgrades.
- Mock GitHub webhook and API sequences, including inbox crash points and failures after remote success.
- Execution-epoch fencing against stale workers, Runtime Processes, MCP calls, and finalization.
- MCP completion barriers during cancellation, connection loss, and process failure.
- Fake human control acquisition, History Replay, prompting, and release back to automation.
- Full Developer and Reviewer state machine using deterministic fake agents.

### Live Tests

- Two real GitHub Apps installed on a disposable repository.
- A real provider credential and OpenCode Runtime Profile.
- Duplicate delivery and webhook redelivery.
- One requested-change cycle.
- One approval and human handoff.
- Orchestrator restart during an active Agent Turn.

## Completion Definition

The first iteration is complete when every acceptance criterion in the specification is automated where practical, the live Developer and Reviewer flow reaches `omnigrex:pr-ready`, the same sessions survive fresh containers, and the documented Compose deployment can reproduce the result.
