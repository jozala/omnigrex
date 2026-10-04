---
status: accepted
date: 2026-10-04
decision-makers: [Mariusz]
---
# Use current-deployment agent instructions

## Context

A new Omnigrex release may change tools and their operational contracts.
Replaying an older persisted effective instruction bundle could direct an agent to use behavior that the current orchestrator no longer supports.

## Decision

Use code-owned instructions from the running Omnigrex binary and load optional operator common and per-Role files into memory once at startup.
Compose these current sources for each Runtime Process launch, including launches continuing a retained Agent Session.
Do not persist the composed instructions as a source for later execution.

## Rationale

This keeps platform instructions aligned with the active deployment and provides a predictable restart boundary for operator edits.
Reading files on every launch is possible but would permit uncoordinated mid-deployment changes; persisted replay would conflict with the freshness requirement.

## Consequences

Operator edits take effect after orchestrator restart, not through hot reload inside an already executing Agent Turn.
Existing repository Agent Profile provenance is separate and retains its current contract.
Tests must prove that a retained Agent Session receives new deployment instructions on its next launch.
See the [implementation plan](../plans/2026-10-04-instruction-ownership-and-handoffs.md).
