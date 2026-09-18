---
status: accepted
---

# Separate Stage Assignments from Agent Participants

A Stage Assignment identifies which Agent Participant performs one Stage in one Assignment Generation.
An Agent Participant identifies one Agent Profile participating in one Workflow and Assignment Generation and owns the immutable Runtime Profile binding and Agent Session lineage.
Conflating these concepts would create duplicate Sessions when multiple Stages select the same Profile and would require mutable assignment lifecycle states.

## Consequences

Agent Participant identity is unique by Workflow, Assignment Generation, and Agent Profile name.
Stage Assignment identity is unique by Workflow, Assignment Generation, and Stage ID and is immutable after creation.
A Stage Assignment may only select a Participant whose Profile Role matches the Stage Role.
Participants and Stage Assignments are provisioned lazily when a Stage first needs them.
Multiple Stages that select the same Profile reuse one Participant and its Agent Session lineage.
A new Assignment Generation creates new immutable identities without rewriting earlier generations.
Agent Session control follows [ADR 0003](./0003-orchestrator-owns-session-control.md).
