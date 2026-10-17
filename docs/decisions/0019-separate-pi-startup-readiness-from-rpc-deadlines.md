---
status: proposed
date: 2026-10-12
decision-makers: [Implementation agent]
---
# Separate Pi startup readiness from ordinary RPC deadlines

## Context

The process transport opens before repository tool installation and the Pi runner finish starting.
The first control-plane RPC can therefore spend its entire ordinary thirty-second request budget waiting for bootstrap, even when the Agent Turn has hours remaining.
An observed Microsandbox candidate failed during repository bootstrap after a repository mise declaration was added, without evidence that Pi had reached its RPC loop.
Retained Participants use their exact images, so a newly required guest readiness marker would be incompatible.

## Decision

Propose a configurable ten-minute Pi startup budget beginning when the adapter attaches to the started Runtime Process.
One absolute deadline covers initial configuration delivery, the first successful `get_state`, and the remainder of checked preparation as an outer limit.
The first validated `get_state` is the readiness barrier; later preparation and configuration RPCs retain the ordinary thirty-second per-request budget.
The caller's deadline always takes precedence if shorter.
No prompt is admitted until exact Session identity, configuration, authority, and durable binding checks complete.

## Rationale

Increasing the ordinary RPC timeout would delay detection of an unresponsive initialized runtime.
Increasing the overall Agent Turn budget would not address the shorter RPC deadline.
Using the existing Pi response avoids changing the pinned guest runner, retained Runtime Profiles, or either execution backend.

## Consequences

Operators can adjust `OMNIGREX_PI_STARTUP_TIMEOUT` for repository tool installation without extending ordinary RPCs or short diagnostic caller budgets.
Startup readiness logs classify timeouts, transport loss, and configuration failures without disclosing repository installation output or credentials.
Stopping uncertain execution and excluding overlapping replacements remain the existing execution owner's responsibilities.

## Links

- [Corrective implementation plan](../plans/2026-10-12-pi-startup-readiness-budget.md).
- [Owned runner and bounded configuration delivery](0014-own-pi-resource-loading-with-a-pinned-runner.md).
- [Existing Agent Turn settlement policy](0016-qualify-pi-turns-with-existing-settlement-policy.md).
