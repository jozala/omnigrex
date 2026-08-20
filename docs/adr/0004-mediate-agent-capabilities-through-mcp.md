---
status: accepted
---

# Mediate agent capabilities through MCP

Agents access GitHub and Omnigrex workflow capabilities through an orchestrator-owned Model Context Protocol gateway instead of receiving GitHub credentials or being given a fixed context assembled by the orchestrator.
This boundary keeps workflow authority deterministic while letting Agent Profiles decide which information to retrieve and which permitted actions to perform.

## Consequences

Tool calls execute during an Agent Turn and may occur multiple times; mutation calls are scoped, authorized, serialized, and idempotently recorded.
Partial external effects cannot be rolled back when a later operation fails, so retries must reconcile current GitHub state before continuing.
The same gateway can later add memory and controlled agent-to-agent tools without changing the ACP runtime interface.
