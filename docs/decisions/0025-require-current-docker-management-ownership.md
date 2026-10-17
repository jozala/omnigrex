---
status: accepted
date: 2026-10-17
decision-makers: [Implementation agent]
---
# Require current Docker management ownership

## Context

The supported release baseline requires fresh installation storage and rejects earlier runtime/database histories.
Every supported Docker Runtime Process is created with the current management marker and immutable execution labels.
Inventory and exact-label cleanup still accepted complete execution labels without that marker to recognize pre-marker resources.

## Decision

Require the current Runtime Process management marker before admitting a container to managed Turn inventory or exact-label/orphan cleanup.
Complete execution labels without the marker do not authorize adoption or mutation.
Keep diagnostic namespace, probe identity, and expiry as a separate ownership contract.
Keep full execution-key matching, immutable Docker-ID revalidation, dispatch fencing, duplicate detection, and malformed marked-resource handling.

## Rationale

The pre-marker fallback supports resources outside the fresh baseline and gives the same identity labels two ownership interpretations.
Explicit management ownership plus exact identity is simpler and matches the supported creation path.
Retaining the fallback to collect old resources would turn normal recovery into historical-installation cleanup.
The operator's explicit disposal procedure already owns that responsibility.

## Consequences

Unmarked containers remain outside managed inventory and are left untouched by these cleaners, even if they carry complete execution labels.
Current marked resources retain normal recovery, orphan cleanup, and lifecycle behavior.
This changes no backend resource label names, retained-state paths, image bindings, or Pi state formats.

## Links

- [Clean supported release baseline](0024-establish-a-clean-supported-release-baseline.md).
- [Execution Backend ownership](0005-separate-execution-backends-from-coordination.md).
- [Implementation and verification](../plans/2026-10-17-participant-compatibility-cleanup.md).
