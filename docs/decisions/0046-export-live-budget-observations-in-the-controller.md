---
status: accepted
date: 2026-10-27
decision-makers: [Implementation agent]
---
# Export live budget observations in the controller

## Context

[Work Item 112](https://github.com/jozala/omnigrex/issues/112) needs aggregate budget pressure and usage without revisiting [0045's enforcement policy](0045-enforce-hidden-per-turn-step-budgets.md).
Its predecessor already supplies bounded owned RPC observations and a critical neutral outcome snapshot.
Persisting SDK custom Session entries would add volume and replay obligations without improving enforcement evidence.

## Decision

Reuse the live transport and neutral Agent Event boundary without custom Session entries or in-Pi exporters.
Separate a one-slot coalescing observation queue from critical runtime events and retain the critical snapshot independently.
Consume events once per prompt in a constant-space controller observer and use the final snapshot for usage and missed summary pressure.
Record incomplete-work handoffs after successful fenced settlement, not speculative reconciliation or summary entry.
Export through the existing optional OpenTelemetry provider with fixed categories and numeric ceilings as histogram values.
Retain unknown usage explicitly, including when the effective configuration is known.
Limit termination dimensions to coverage counters and ceiling dimensions to Role.
Increase the classic-series target to 600 (531 application series plus metadata allowance), with executable budget tests.

## Rationale

A second counter or transcript-derived usage would drift from enforcement.
Durable usage storage is unnecessary for the requested operator analysis and could imply unimplemented exactly-once accounting.
The SDK periodic exporter separates network failures from work without adding a network dependency inside Pi or settlement.
The explicit cardinality increase preserves Role, budget mode and completeness rather than mixing partial and exact usage.

## Consequences

Telemetry is best-effort live observation, not a usage ledger.
Controller death, export failure and uncertain settlement acknowledgement can lose samples.
History Replay never re-emits live metrics or resets the budget.
No owned runtime resources or immutable Participant bindings change.
Future observation types can reuse this boundary; unrelated Pi metrics remain out of scope.
See the [instrument and delivery contract](../observability/application.md#agent-turn-step-budget-metrics).
