---
status: accepted
date: 2026-10-07
decision-makers: [Mariusz]
---
# Use Pi through an agent-specific adapter

## Context

Session control and Workflow behavior must remain independent of Pi's protocol mechanics.
Pi RPC has runtime-specific session identity, prompt completion, replay, and configuration semantics.
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
Protocol-specific types in coordination would obscure Pi's admission-versus-settlement distinction.
Native Pi MCP is a credible alternative, but its presence alone does not qualify the required authority, resource-exclusion, and tool-selection behavior.

## Consequences

Session Continuation and History Replay remain separate capabilities, and Runtime Profiles remain immutable.
Pi-to-Pi qualification and retained-image availability remain required.
An interface does not promise cross-agent conversation migration.

## Links

- [Contract design](../plans/2026-10-07-runtime-boundary-contracts.md)
- [Superseded ADR 0002](../plans/runtime-history/adr-0002.md)
- [Pi evidence](../experiments/pi.md)
- [Pi runtime Work Item](https://github.com/jozala/omnigrex/issues/33)
