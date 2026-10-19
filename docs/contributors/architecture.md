# Architecture overview

[Contributor guide](README.md) · [Domain language](../../CONTEXT.md)

Omnigrex separates durable Workflow coordination from runtime-specific agent execution.
GitHub holds human-readable work and review artifacts; PostgreSQL holds the operational ledger, jobs, identities, and mutation/recovery state.

```text
GitHub webhooks -> durable intake -> Workflow coordination -> Agent Turn
      ^                                      |                  |
      |                                      v                  v
GitHub Apps <- trusted MCP capabilities <- scoped gateway <- Pi runtime
                                             |
                              Docker or Microsandbox backend
```

## Main boundaries

| Area | Responsibility |
| --- | --- |
| `cmd/omnigrex/` | Process composition, lifecycle, and operational commands |
| `internal/workflow/`, `internal/workflowaction/` | Code-defined Stage graph, state transitions, and durable effects |
| `internal/store/` | PostgreSQL ledger, jobs, leases, and forward-only migrations |
| `internal/github/` | Apps, webhook intake, reads, and external mutations |
| `internal/agentturn/` | Preparation, execution, settlement, and recovery |
| `internal/runtime/` | Runtime Profiles, Sessions, Pi adapter, and backend contracts/implementations |
| `internal/mcp/` | Scoped capability authorization and trusted mutation handling |
| `internal/workspace/` | Repository preparation, captures, and publication |
| `agent/pi/` | Owned non-root runner, configuration/resource boundary, and fixtures |
| `.omnigrex/team/` | Repository Agent Profiles for Omnigrex dogfooding |

A Work Item has a Workflow and bounded Workflow Attempts.
Stage Assignments select durable Agent Participants with their own Agent Sessions and immutable Runtime Profiles.
An Execution Backend creates disposable environments; it does not own the business Workflow or durable conversation identity.
Repository code runs without GitHub credentials and reaches business capabilities through authenticated, scoped MCP.

## Read the decisions behind a change

- [Visible versus execution state](../adr/0001-separate-visible-workflow-and-execution-state.md).
- [Orchestrator-owned Session control](../adr/0003-orchestrator-owns-session-control.md).
- [MCP-mediated capabilities](../adr/0004-mediate-agent-capabilities-through-mcp.md).
- [Independent GitHub App identities](../adr/0005-use-separate-developer-and-reviewer-github-apps.md) and [operation-selected credentials](../adr/0009-select-github-app-credentials-by-operation.md).
- [Code-defined Workflow Definitions](../adr/0007-use-code-defined-workflow-definitions-and-durable-attempt-stages.md) and [Stage Assignments versus Participants](../adr/0008-separate-stage-assignments-from-agent-participants.md).
- [Execution backend boundary](../decisions/0005-separate-execution-backends-from-coordination.md) and [Pi adapter](../decisions/0006-use-pi-through-an-agent-specific-adapter.md).
- [Repository Profile discovery](../adr/0011-discover-repository-agent-profiles-and-select-separately.md) and [working-checkout skills](../adr/0012-load-repository-skills-from-the-current-checkout.md).

Use the [decision index](../decisions/README.md) for status and supersession, and [plans](../plans/README.md) for accepted intent and delivery evidence.
Historical OpenCode/ACP designs do not describe the current Pi-only runtime.
