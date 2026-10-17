---
status: accepted
date: 2026-10-07
decision-makers: [Mariusz]
---
# Separate execution backends from coordination

## Context

Replacing Docker with Microsandbox currently reaches beyond process launch into recovery, storage, diagnostics, and deployment assumptions.
Mariusz wants another implementation, including a future remote service, to be introducible without rewriting Workflow or Agent Turn policy.
Keeping only a launcher interface would leave shared-filesystem and restart-recovery dependencies in core code.

## Decision

Retain Docker and Microsandbox behind backend-neutral execution, inventory, workspace, state, and diagnostic contracts.
Select one backend per deployment.
Keep fencing, retry decisions, MCP authority, and settlement in the orchestrator.
Keep concrete SDKs, resource identifiers, host paths, and network mechanics inside adapters and their composition layer.

Both implementations must pass the same core conformance suite.
Docker remains supported for trusted repositories; Microsandbox additionally qualifies hardware isolation and nested-container development on Linux/KVM.
Unsupported requirements fail explicitly rather than selecting a weaker configuration or another backend.

## Rationale

Two retained implementations provide evidence that the boundary is real.
A Microsandbox-shaped interface would merely relocate vendor coupling.
A universal platform framework would add unnecessary capabilities beyond Omnigrex's lifecycle and recovery needs.
Future remote adapters still own their provider integration, artifact transport, and management authentication.

## Consequences

This supersedes ADR 0006's Docker-only execution architecture, while preserving its warning about Docker management authority and prohibition on giving agents the host socket.
Backend replacement does not imply automatic cross-backend state migration.

## Links

- [Contract design](../plans/2026-10-07-runtime-boundary-contracts.md)
- [Superseded ADR 0006](../adr/0006-control-docker-directly-from-the-orchestrator.md)
- [Microsandbox evidence](../experiments/microsandbox.md)
- [Migrate Agent Turn isolation from Docker containers to Microsandbox](https://app.clickup.com/t/124ckhdev73)
