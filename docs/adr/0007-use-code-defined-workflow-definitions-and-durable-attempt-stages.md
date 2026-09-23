---
status: accepted
---

# Use code-defined Workflow Definitions and durable Attempt Stages

Every deployment constructs one immutable Workflow Definition in code.
The Definition owns stable Stage IDs, Stage-to-Role bindings, accepted Agent Turn purposes, outcome transitions, and per-Stage review limits.
Workflow Attempts persist their current Stage and per-Stage review usage, but they do not persist a Definition ID, version, or snapshot.

## Consequences

The Workflow Reducer is constructed from the deployment Definition and injected into persistence and event-processing boundaries.
Agent Turns record their Stage so retries, recovery, settlement, and audit do not infer responsibility from mutable Workflow state names.
Startup validates every referenced Role and Role Policy before work is accepted.
Repository-specific Agent Profile discovery and selection are validated against the exact default-branch commit during preflight and Agent Turn preparation, as described in [ADR 0011](./0011-discover-repository-agent-profiles-and-select-separately.md).
An incompatible Definition change that leaves durable state without a known Stage or outcome fails closed into Human Handoff rather than guessing a transition.
Operators must coordinate incompatible code changes with a database reset or an explicit data migration.
