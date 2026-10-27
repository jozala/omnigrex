---
status: accepted
date: 2026-10-28
decision-makers: [Implementation agent]
---
# Classify telemetry failures without error prose

## Context
The global OpenTelemetry error handler receives SDK errors without a signal or collection/export context.
The pinned v1.35.0 HTTP exporters wrap transport errors, but their HTTP rejection errors retain status only in formatted text alongside endpoint URLs and arbitrary response bodies.
Partial-success responses are reported separately through the global handler as an internal SDK struct, even when the export itself succeeds.

## Decision
Log bounded classifications through the existing local operational logging path.
Use narrow exporter wrappers for known signal/export context, the existing durable-observation callback for collection context, and provider shutdown boundaries for shutdown context.
Keep global errors explicitly unknown unless trusted typed evidence establishes more.
Recognize the exact pinned SDK partial-success struct by package path and type name, extracting only its validated nonnegative rejected-item count and signal from the type's package.
Bound error-chain traversal and fall back to unknown on extraction failure.
Do not parse error prose, even to recover an HTTP status.

## Rationale
Parsing formatted HTTP errors would couple classification to strings containing remote text and credentials.
Replacing the HTTP transport, copying exporter code, or upgrading dependencies solely to install response hooks would expand this focused correction and risk altering delivery or retry behavior.
Exact internal-type inspection preserves useful partial-success evidence without accessing its arbitrary message or requiring unsafe Go features.
This compatibility dependency is covered by local exporter fixtures and must be requalified on SDK upgrades.

## Consequences
HTTP rejections without typed evidence remain `error_kind=unknown` at the known signal/export boundary; no HTTP status is fabricated.
Uninstrumented collection callbacks remain unknown at the global handler rather than being mislabeled as exports.
An SDK-reported export failure can produce both a signal-specific boundary record and an unknown global record; deduplication by timing or mutable shared state is deliberately avoided.
Returned errors, retry policy, intervals, and provider shutdown semantics remain unchanged.

## Links
- [Work Item 143](https://github.com/jozala/omnigrex/issues/143)
- [Operator observability](../operators/observability.md#telemetry-sdk-diagnostics)
