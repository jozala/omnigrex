# First Iteration

Status: Accepted

## Purpose

The first iteration validates that a deterministic orchestrator can coordinate persistent Developer and Reviewer agent sessions through GitHub without embedding the software-development decisions inside the orchestrator.

The workflow is:

```text
Human adds omnigrex:run to an Issue
  -> Developer works in its persistent session
  -> Developer opens a normal Pull Request
  -> Reviewer reviews in a separate persistent session
  -> Developer and Reviewer iterate when changes are requested
  -> Reviewer approves
  -> Omnigrex adds omnigrex:pr-ready
  -> Human takes over
```

## Goals

- Receive and authenticate GitHub App webhooks.
- Coordinate one Developer Role and one Reviewer Role per Issue.
- Execute agents through Agent Client Protocol version 1.
- Preserve one resumable Agent Session per Agent Assignment.
- Let agents retrieve current context and perform controlled actions through Model Context Protocol tools.
- Keep GitHub credentials exclusively in the orchestrator.
- Persist operational state and recover safely after process restarts.
- Run the complete system with Docker Compose and non-root container processes.
- Preserve an architecture that can later support human control of an existing Agent Session.

## Non-goals

- Product Owner, Architect, or QA Roles.
- Dynamic hiring or dynamic workflow composition.
- Multiple agents performing the same Role on one Issue.
- Direct agent-to-agent communication.
- Cross-Issue Agent Session memory.
- Durable organizational memory written by agents.
- A human takeover UI.
- Runtime-neutral transcript persistence.
- More than one ACP agent implementation.
- Remote ACP transports.
- Automatic merging.

## Work Initiation

A human starts or resumes automation by adding `omnigrex:run` to a GitHub Issue.

The orchestrator consumes the command label, creates or reactivates the Issue's Workflow, and creates a new Workflow Attempt.
Webhook redelivery or repeated labeling must not create duplicate active attempts.

If an open Omnigrex Change Proposal already exists, the new Workflow Attempt reuses it and resumes the relevant Agent Assignment.
A new Workflow Attempt resets the three-Review-Cycle budget but does not replace existing Agent Sessions.

## Visible Workflow State

GitHub exposes the following labels:

- `omnigrex:run`
- `omnigrex:developing`
- `omnigrex:reviewing`
- `omnigrex:pr-ready`
- `omnigrex:needs-human`

The orchestrator creates missing labels and keeps exactly one state label active on the Issue and, after creation, its Pull Request.
Labels are the human-visible workflow state, while PostgreSQL is the operational execution ledger.

## Agent Assignments

Each Issue has at most one active Developer Assignment and one active Reviewer Assignment in the first iteration.

Each Agent Assignment binds:

- The Issue.
- A Role.
- An Agent Profile identity whose version may be refreshed between turns.
- An immutable Runtime Profile selected when the Assignment is created.
- One active Agent Session.

Developer and Reviewer sessions are independent.
The same Developer Session is used for initial implementation, requested changes, retries, and later Workflow Attempts for that Issue.
The same rule applies to the Reviewer Session.

Agent Sessions are never shared across Issues.

## Agent Runtime

ACP version 1 is the runtime protocol between the orchestrator and an agent implementation.
OpenCode, started with `opencode acp`, is the only supported agent implementation in the first iteration.

The core runtime model treats Session Continuation and History Replay as separate capabilities.
It does not require a particular ACP method.

The OpenCode Runtime Profile requires both capabilities and session discovery for creation recovery.
The ACP adapter may implement Session Continuation with `session/resume`, or with `session/load` while discarding replayed history.
It implements History Replay with `session/load`.

The Runtime Profile is immutable and versioned.
It defines the image digest, ACP command, environment contract, persistent state paths, required protocol capabilities, supported platform, and stable workspace path.

Every OpenCode Runtime Process sees its assignment workspace at exactly `/workspace`.
The absolute path is part of session compatibility and cannot change during an Agent Session.

Runtime Processes are disposable.
The runtime-specific session state is persistent and mounted separately from the refreshed workspace.

## ACP Client Responsibilities

The orchestrator is the sole ACP client and the only gateway through which autonomous code or a future human UI may submit prompts.

For each Runtime Process, the orchestrator:

1. Starts a non-root container and attaches ACP over standard input and output.
2. Negotiates ACP version and capabilities.
3. Creates or continues the Agent Session.
4. Supplies the Omnigrex MCP server configuration.
5. Applies the Agent Profile's runtime configuration.
6. Sends a deterministic event envelope with the expected outcome.
7. Processes session updates, usage, tool status, permission requests, cancellation, and the final stop reason.
8. Stops and removes the Runtime Process after the active Agent Turn.

The first iteration does not advertise ACP client filesystem or terminal capabilities.
OpenCode operates directly in the mounted workspace using its local tools.

Autonomous permission requests are answered deterministically from the Agent Profile.
Unknown permission requests are denied.
The ACP client fails closed and never advertises filesystem or terminal capabilities that the orchestrator has not implemented.

If the `session/new` response is lost after OpenCode creates persistent state, the adapter uses assignment-isolated session discovery to recover the only matching session instead of creating another one.

## Tool-first Context

The orchestrator does not assemble Issue bodies, comments, reviews, checks, or findings into prompts.

Every Agent Turn receives a small event envelope containing:

- Repository identity.
- Issue number.
- Pull Request number when one exists.
- Triggering event.
- Current Pull Request head when relevant.
- Expected outcome.
- Available capabilities.

The agent decides what context it needs and retrieves current information through MCP tools.
The orchestrator validates durable outcomes but does not validate which read tools the agent chose to call.

## MCP Tool Gateway

The Omnigrex MCP Tool Gateway runs inside the orchestrator and is available only on the private agent network.
Every tool call is authenticated with a scoped capability token and authorized against the current Agent Assignment.

Read tools for the first iteration are:

- `get_issue`
- `list_issue_comments`
- `get_pull_request`
- `list_pull_request_reviews`
- `list_review_threads`
- `get_check_runs`

Mutation and workflow tools are:

- `publish_changes`
- `open_pr`
- `request_review`
- `submit_review`
- `comment_on_issue`
- `comment_on_pull_request`
- `report_blocked`

`request_review` records an internal Omnigrex handoff to the Reviewer and does not depend on GitHub accepting an App bot as a requested reviewer.

An agent may make zero, one, or many tool calls during an Agent Turn.
Calls execute synchronously during the turn and return their results to the agent.

Mutation calls are idempotent, serialized per Agent Session, and recorded in the execution ledger.
The orchestrator performs GitHub mutations under the appropriate GitHub App identity.
Agents never receive GitHub credentials.

GitHub webhooks caused by tool calls are persisted immediately, but they cannot start a competing Agent Turn while the current turn remains active.
Before each external mutation, the orchestrator reserves a stable operation identifier and records the invocation as `RESERVED`, `IN_FLIGHT`, `UNKNOWN`, `SUCCEEDED`, or `FAILED`.
Reconciliable GitHub artifacts include the operation identifier, and Git pushes use an expected old head and deterministic commit data.

When the ACP prompt ends, the orchestrator closes mutation admission, waits for every admitted invocation to reach a terminal state, reconciles every `UNKNOWN` result, revokes the tool token, and only then finalizes the Agent Turn.
If an `UNKNOWN` result cannot be reconciled immediately, the Agent Turn enters `RECONCILING`, releases its Runtime Process and concurrency slot, and blocks successor scheduling while a durable job retries with a bounded budget.
Unresolved uncertainty creates a Human Handoff rather than allowing an unbounded wait or a possibly conflicting successor.
After this completion barrier, the orchestrator reconciles GitHub and workspace state and schedules at most one next turn.

## Developer Behavior

The initial Developer Turn begins from the repository's current default branch.
The Developer uses tools to inspect the Issue, changes files in the workspace, publishes changes, and opens a normal Pull Request.

A returning Developer starts with the current Pull Request head checked out at `/workspace` and receives only the triggering event envelope.
The Developer decides whether to inspect review threads, publish changes, explain disagreement, request another review, or report a blocker.

A successful Developer outcome is one of:

- A Pull Request is opened and review is requested.
- Changes are published and review is requested.
- A durable response is published and review is requested without a code change.
- A blocker is reported.

When publishing through a clean checkout, unpublished changes mean that the normalized agent workspace tree differs from the tree of the latest pushed head.
A successful Developer outcome requires those trees to match after the MCP completion barrier.

## Reviewer Behavior

The Reviewer receives the current Pull Request head in its workspace and retrieves Issue, Pull Request, review, and check context through MCP tools.
The Reviewer receives no file-editing tools and cannot publish repository changes.
Local verification commands may create temporary files, but all Reviewer workspace changes are discarded after the turn.

The Reviewer App submits native GitHub reviews under an identity distinct from the Developer App.

A successful Reviewer outcome is one of:

- Approve the current Pull Request head, optionally with Non-blocking Findings.
- Request changes with one or more Blocking Findings.
- Report a blocker.

The orchestrator accepts a Reviewer outcome only when it was submitted by the active Reviewer Assignment's App identity for the expected Pull Request head and that head remains current during outcome reconciliation.
A review made stale before acceptance does not consume a Review Cycle.
An accepted approval stores `ready_for_sha` before adding `omnigrex:pr-ready`.
Every Pull Request synchronization invalidates mismatched readiness and removes the advisory label during reconciliation.
If the review budget is exhausted when a later synchronization requires another review, the Workflow creates a Human Handoff.

An accepted approval adds `omnigrex:pr-ready` and hands the Pull Request to a human.
The label is eventually consistent with `ready_for_sha` because GitHub cannot atomically compare the head and update a Pull Request label.

## Review Limit

A Workflow Attempt allows at most three Review Cycles.
A Review Cycle is counted only when a Reviewer review is successfully published for the expected Pull Request head.
Failed Agent Turns and infrastructure retries do not consume the review budget.

After the third change-requesting review, the orchestrator stops autonomous work and adds `omnigrex:needs-human`.
Re-adding `omnigrex:run` starts a new Workflow Attempt, resets the review budget, and continues the existing Agent Sessions.

## Failures and Blockers

A failed or timed-out Agent Turn is retried once in the same Agent Session.
After the second failure, the orchestrator publishes diagnostics and creates a Human Handoff.

Calling `report_blocked` creates a Human Handoff without an infrastructure retry.

Partial MCP and GitHub side effects are not rolled back.
The orchestrator reconciles current durable state before retrying and uses idempotency markers to avoid duplicate Pull Requests, comments, commits, and reviews.

Every Agent Turn receives a monotonically increasing execution epoch.
Runtime Process labels, MCP tokens, external mutations, and turn finalization carry that epoch.
An expired lease or newer epoch prevents stale workers from starting processes, calling mutation tools, or finalizing turns, and stale Runtime Processes are terminated.
Before a newer epoch may perform side effects, the orchestrator closes mutation admission for the prior epoch, stops its Runtime Process, and settles or escalates every admitted invocation.

An orchestrator restart never adopts an in-flight ACP prompt because the new process does not own its JSON-RPC correlation state.
The orchestrator fences and stops the old Runtime Process, drains or reconciles admitted mutations, and retries through a fresh Runtime Process using the same Agent Session.

## Session Control

Assignment status and session control authority are separate concepts.

Assignment statuses are:

- `ACTIVE`
- `WAITING_FOR_HUMAN`
- `COMPLETED`
- `SUPERSEDED`

Control owners are:

- `AUTOMATION`
- `HUMAN`

The first iteration uses only the `AUTOMATION` owner but persists a control revision that prevents two controllers from submitting competing prompts.
A future human takeover must first fence autonomous scheduling and wait for or cancel the active Agent Turn before acquiring control.

ACP agent modes such as planning or coding are runtime configuration and are not session control authority.

## Session State and Retention

Runtime-specific Agent Session state is retained because Session Continuation requires it.
It may include conversation and tool history owned by OpenCode.

PostgreSQL stores only the ACP session identifier, runtime binding, capabilities, storage location, lifecycle, and operational metadata.
The first iteration does not duplicate transcript content into PostgreSQL or GitHub.

Runtime-neutral Agent Event persistence is deferred rather than rejected.
The runtime layer emits normalized Agent Events internally so a future event store can support debugging, auditing, analytics, and UI replay.

When an Issue closes, its assignments become completed and their runtime state is retained for 30 days by default.
The retention duration is deployment-configurable.
Issue closure fences new Agent Turns, waits for or cancels an active turn, and reconciles admitted mutations before completing the Assignments.
Reopening the Issue before garbage collection cancels deletion but does not restart automation.
The completed Assignments and their Agent Sessions become active again only when a new Workflow Attempt is accepted.
If the Issue is reopened or triggered after garbage collection, Omnigrex creates new Assignments and Agent Sessions while retaining the prior Workflow history.

Workspaces may be deleted immediately after an Agent Turn.
Opaque runtime session state is deleted by an idempotent garbage collector only after the retention deadline.
PostgreSQL Workflow, Assignment, Session, Turn, and tool-invocation records remain as operational history with the runtime state marked deleted.

## Runtime Upgrades

An Agent Session remains pinned to the immutable Runtime Profile version and image digest that created it.
Deploying a new OpenCode image does not automatically migrate existing sessions.

A candidate Runtime Profile upgrade is considered compatible only after a controlled test proves Session Continuation and History Replay across the exact source and target image digests, platform, state contract, and `/workspace` path.
The old image remains available while retained sessions still reference it.
Runtime state must be copied before a candidate version opens it because runtime migrations may be irreversible.
The first iteration records compatibility results but never changes an existing Agent Session's Runtime Profile binding automatically.
Replacing the runtime implementation creates a new Agent Session.

## GitHub Applications

The first iteration uses two GitHub Apps.

The Developer/Orchestrator App:

- Receives signed webhooks.
- Reads and writes Issues and Pull Requests.
- Reads and writes repository contents.
- Reads check runs.
- Reads and writes GitHub Actions workflow files.
- Manages workflow labels and comments.
- Creates installation-scoped tokens for orchestrator Git operations.

The Reviewer App:

- Reads repository contents and Pull Requests.
- Submits native reviews under a distinct identity.
- Has its GitHub App webhook explicitly disabled.

Both Apps must be installed on every managed repository.
Missing installation or permissions create a Human Handoff.

## Repository Configuration

Managed repositories configure the two Agent Profiles at:

```text
.omnigrex/team/developer.md
.omnigrex/team/reviewer.md
```

Each file contains validated YAML front matter followed by Role instructions.
The front matter selects a Runtime Profile and its model, variant, steps, and permissions configuration.
Provider credentials never appear in the repository.

The orchestrator loads Agent Profiles from the latest default branch before every Agent Turn.
Changes on the feature branch cannot alter the Reviewer Profile used to review that branch.
The Agent Profile identity is stable while its instructions, model, variant, steps, and permissions may change between turns.
The Assignment remains pinned to its original Runtime Profile; changing the profile's runtime reference for an existing Assignment creates a configuration Human Handoff instead of migrating the Agent Session.
The OpenCode adapter renders the active Agent Profile into a read-only, runtime-owned configuration with precedence over repository OpenCode settings.
Reviewer Runtime Processes disable project OpenCode plugins, MCP servers, agents, and instruction discovery from the feature branch and inject only approved configuration and instructions from the default branch.
The feature-branch files remain available for inspection as source code but cannot alter the Reviewer's effective capabilities.

The common agent image includes OpenCode, Git, shell utilities, and a pinned mise installation.
If a repository contains mise configuration, its tools are installed and activated before the Agent Turn.
Developer tool provisioning uses the Developer workspace revision, while Reviewer tool provisioning uses the latest default-branch mise configuration and lock data.
Feature-branch mise changes remain visible to the Reviewer as source but cannot execute or alter the Reviewer's tool environment.
Repositories without mise configuration receive only the base image tools.

Custom repository development images are outside the first iteration.

## Operational State

PostgreSQL persists at least:

- Webhook deliveries.
- Workflows and Workflow Attempts.
- Agent Assignments.
- Agent Sessions and Agent Turns.
- Pull Request associations.
- MCP tool invocations.
- Durable jobs, attempts, and leases.

Webhook deliveries are acknowledged only after durable insertion into an inbox with processing state and a claim lease.
Normalization, state mutation, and successor job creation are committed transactionally, so a crash after acknowledgement cannot lose the transition.
Delivery identifiers, execution epochs, state-transition guards, row locks, and leases make event handling idempotent and recoverable.

The orchestrator reconciles stale leases and labeled Runtime Processes after restart.

## Deployment and Isolation

Docker Compose runs the Go orchestrator and PostgreSQL and builds the common agent image.
The orchestrator exposes HTTP behind an operator-provided HTTPS proxy or tunnel.

The orchestrator mounts the Docker socket directly and is therefore trusted with effective host-root authority even though its process runs as a non-root user.
Agent containers never receive the Docker socket.

All containers run non-root processes.
Agent containers use dropped capabilities, `no-new-privileges`, resource limits, a read-only root filesystem, isolated workspace subpaths, and temporary writable directories.
Runtime Profiles enumerate every writable HOME, XDG, cache, state, temporary, workspace, and tool directory required by the runtime.
Assignment subdirectories are created and ownership-checked before Docker mounts them.
The first iteration requires Docker Engine API 1.45 or newer for volume subpath support.

Only trusted repositories are supported in the first iteration.
Provider credentials are available to the agent runtime and may be exposed by malicious repository code or instructions.

One deployment handles all repositories available to its GitHub App installations.
Agent Turn concurrency is globally configurable and defaults to two.

## Acceptance Criteria

- A signed `omnigrex:run` webhook creates one Workflow Attempt despite duplicate delivery.
- The Developer creates a Pull Request through its ACP session and Omnigrex MCP tools without GitHub credentials.
- The Reviewer uses a different ACP session and GitHub App identity.
- A change-requesting review resumes the same Developer Session in a fresh container.
- An approved review adds `omnigrex:pr-ready`.
- Three change-requesting reviews create `omnigrex:needs-human`.
- A failed Agent Turn retries once without consuming the review budget.
- Re-adding `omnigrex:run` reuses sessions and resets the review budget.
- Agent Session continuation works after replacing the Runtime Process and workspace while preserving `/workspace`.
- OpenCode history can be replayed without Omnigrex persisting transcript content.
- A lost `session/new` response recovers the Assignment's existing OpenCode session without creating another one.
- Stale execution epochs cannot submit prompts, mutate GitHub, or finalize Agent Turns.
- An accepted Reviewer approval is bound to the Reviewer App identity and the current Pull Request head.
- Session state survives orchestrator restart and is retained for 30 days after Issue closure.
- Every running container process has a non-zero user ID.
- `docker compose up` starts a healthy stack after secrets and external HTTPS ingress are configured.
