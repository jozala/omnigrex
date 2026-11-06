---
status: accepted
date: 2026-11-06
decision-makers: [Implementation agent]
---
# Sample mounted storage without fleet accounting

## Context
Work Item #183 adds prompt, tool, and storage telemetry to the existing optional orchestrator exporter.
Its governing constraint favors readily available approximate observations over accurate fleet accounting.
Mounted trees can change during traversal and several Participants share a backing filesystem.

## Decision
Reuse the cache monitor's background cadence and metadata-only resumable scanner for four fixed mounted resource categories, independently of its existing retained-cache warning state.
Export distributions of allocated-byte observations and bounded collection statuses, not a retained per-Participant usage ledger or a current fleet total.
Sample backing-filesystem statistics once per observed device per poll, without owner or resource dimensions.
Keep client, native, and admitted server tool measurements in independent fixed layers.
Increase the classic-series target to 2050 for the executable 1952-series application ceiling, allowing 98 additional backend metadata series.

## Rationale
A current fleet gauge would require retaining and expiring owner measurements and defining completeness across changing inventory, which is disproportionate to this Work Item.
Summing filesystem capacity per Participant would misrepresent shared space as exclusive capacity.
Separate measurement layers avoid implying client cancellation ends detached server work.

## Consequences
Storage distributions are samples of concurrent traversals, not atomic snapshots, exact totals, or necessarily lower bounds at completion.
Repeated partial scans can contribute overlapping samples; operators must filter status rather than sum them.
Missing, inaccessible, and failed measurements do not contribute fabricated usage values.
No runtime producer, retention policy, cleanup, or authority changes are introduced.
Live deployment acceptance remains separate.

## Links
- [Work Item 183](https://github.com/jozala/omnigrex/issues/183)
- [Application contract](../observability/application.md#prompt-tool-and-storage-observations)
- [Existing live observation boundary](0053-export-live-budget-observations-in-the-orchestrator.md)
