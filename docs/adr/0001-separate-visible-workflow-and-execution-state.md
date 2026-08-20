---
status: accepted
---

# Separate visible workflow and execution state

GitHub is the durable, human-visible source of truth for Work Items, Change Proposals, findings, and handoffs, while PostgreSQL is the authoritative execution ledger for webhook deliveries, attempts, assignments, sessions, turns, tool calls, leases, and retries.
Using GitHub alone would make duplicate delivery, concurrent work, and crash recovery unsafe, while using PostgreSQL alone would hide workflow state from the project's normal collaboration interface.

## Consequences

Every externally visible transition must update GitHub idempotently, and every GitHub mutation must tolerate its webhook arriving before or after the corresponding internal operation completes.
The orchestrator must reconcile the two representations rather than assume a distributed transaction exists.
Opaque runtime state may be garbage-collected, but PostgreSQL Workflow and execution metadata remains as the operational history.
