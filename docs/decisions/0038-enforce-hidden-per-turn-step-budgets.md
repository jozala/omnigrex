---
status: accepted
date: 2026-10-24
decision-makers: [jozala, Implementation agent]
---
# Enforce hidden per-Turn model-iteration ceilings

## Context

An unbounded model/tool loop can consume installation resources without reaching a durable Stage outcome.
The agreed policy in [Work Item 111](https://github.com/jozala/omnigrex/issues/111) requires Pi-supported enforcement without changing ordinary planning through countdown instructions.
Pi's inner-loop stop decision alone does not stop outer activity continuations.

## Decision

Resolve an optional Profile `max_steps` against an operator fallback at Agent Turn preparation, retaining omission separately from explicit unlimited in canonical Profile snapshots.
Reserve the last finite iteration for an honest, tool-free summary, including when the limit is one.
Keep ordinary requests unaware of numeric allowances.
Chain Pi's public request/finish hooks, project the final request after forced-system-prompt restoration, remove executable tools, and block nested tool access through the owned extension.
Use non-awaited public Session abort at the final response boundary to stop outer continuations as well as the inner loop.
Retain one counter across internal activity and restore configured tools for the next admitted prompt.

Classify incomplete positive budget settlement as immediate Human Handoff, not infrastructure retry.
A summary is not completion evidence: durable terminal MCP intent and existing corroboration remain authoritative, including at the summary boundary.
Provider errors, cancellation, token limits, and transport uncertainty retain their distinct classifications.

## Rationale

Counting tool calls would not bound conversational model iterations.
Allowing N tool-capable iterations plus another summary would exceed the agreed ceiling.
Disclosure might change planning behavior; its benefit is not established for this development Workflow.
Prompt-only enforcement is not a runtime safety boundary.
Treating exhaustion as infrastructure failure would silently replenish the allowance.

## Consequences

Typed bounded observations support individual Turn diagnosis without a telemetry backend or a second counter.
Aggregate telemetry remains assigned to [Work Item 112](https://github.com/jozala/omnigrex/issues/112).
Historical Runtime Profiles remain immutable; finite budgets require qualified budget-capable resources rather than silent image rebinding.
See the [operator contract](../reference/agent-step-budgets.md) for configuration, accounting, compatibility, and recovery.
