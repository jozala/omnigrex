---
status: accepted
date: 2026-10-07
decision-makers: [Mariusz]
---
# Use Pi through an agent-specific adapter

## Context

The planned runtime replacement removes OpenCode, but session-control and Workflow behavior must survive that change.
Pi RPC differs from ACP in session identity, prompt completion, replay, and configuration semantics.
The feasibility experiment proves exact session reopening and persisted-entry replay, and demonstrates that successful configuration commands can still clamp unsupported values.

## Decision

Use Pi RPC behind an adapter exposing Omnigrex's session preparation, checked configuration, History Replay, prompt outcome, cancellation, and Agent Event operations.
Keep Pi protocol types and persisted-entry interpretation inside that adapter.
Keep durable Agent Session control, prompt admission, and mutation reconciliation in the orchestrator.

Use the qualified Omnigrex-owned MCP extension initially, with an exact per-Turn endpoint and authority.
Disable uncontrolled project resource discovery while preserving explicitly selected checkout-local skills and current-deployment instructions.
Reject unsupported model/thinking requests without fallback and verify effective values before prompting.

## Rationale

A narrow semantic contract isolates useful behavior without pretending different agents have interchangeable conversations or completion rules.
Keeping ACP-shaped types would obscure Pi's admission-versus-settlement distinction.
Native Pi MCP is a credible alternative, but its presence alone does not qualify the required authority, resource-exclusion, and tool-selection behavior.

## Consequences

This supersedes ADR 0002's ACP protocol choice, not its separation of Session Continuation and History Replay or its immutable Runtime Profile principle.
The initial cutover resets OpenCode operational/runtime state; it does not convert conversations.
Pi-to-Pi qualification and retained-image availability remain required.
An interface does not promise cross-agent conversation migration.

## Links

- [Contract design](../plans/2026-10-07-runtime-boundary-contracts.md)
- [Superseded ADR 0002](../adr/0002-use-acp-as-the-agent-runtime-boundary.md)
- [Pi evidence](../experiments/pi.md)
- [Replace OpenCode with Pi RPC](https://github.com/jozala/omnigrex/issues/33)
