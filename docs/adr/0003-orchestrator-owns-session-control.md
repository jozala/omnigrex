---
status: accepted
---

# Orchestrator owns session control

The orchestrator is the sole client and control gateway for every Agent Session, including future prompts submitted by a human UI or another agent.
Allowing clients to attach directly to a runtime would make prompt ordering, permission enforcement, cancellation, review limits, and ownership transfer vulnerable to races.

## Consequences

Stage Assignment identity and Control Owner are modeled independently, and every prompt is fenced by a transactional control revision.
A future human UI communicates with the orchestrator rather than the runtime and can acquire control only after an active Agent Turn has completed or been cancelled.
Human control is an explicit claim on one Agent Session, and at most one Agent Session in a Workflow may be human-controlled at a time.
ACP agent modes remain runtime configuration and do not represent control ownership.

Agent Sessions are owned by Agent Participants as described in [ADR 0008](./0008-separate-stage-assignments-from-agent-participants.md).
