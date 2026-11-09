---
status: accepted
date: 2026-11-09
decision-makers: [Implementation agent]
---
# Observe live tools and temporary filesystems without accounting state

## Context
Work Item #184 extends the shared recording contract delivered by #183 and requires readily available runtime evidence rather than a usage ledger.
Pi 1.4.0 exposes optional monotonic tool execution duration, and Node supplies asynchronous filesystem statistics.
Historical Participants retain their exact image/resource contracts.

## Decision
Preserve bounded terminal tool evidence on existing activity events and consume it through the shared native/client tool recorder.
Use a single sequential asynchronous `statfs` sampler for the two owned temporary mounts, with best-effort stdout delivery and an independent one-slot coalescing adapter queue.
Keep the budget queue and critical snapshot independent.
Record filesystem consumption, not directory allocation, and extend the shared filesystem histograms with a dimensionless `used` byte distribution.
The current 27-name MCP catalog also includes `assess_human_input`, previously missing from the recording vocabulary.
The used-byte instrument adds 11 classic series and the catalog correction adds 36, bringing the application ceiling to 1999 within the existing 2050 target.

## Rationale
Pairing starts and endings, retaining exact last values, and synthesizing timing from transport arrivals would add state without establishing accurate accounting.
Async metadata-only calls avoid commands, directory traversal and credential access; single-flight scheduling bounds hung work without blocking the JavaScript input path.
A separate observation slot prevents temporary data from displacing budget or critical Turn evidence.
Reusing distributions avoids a retained freshness ledger and leaves stopped processes without misleading current gauges.

## Consequences
Missing/malformed timing omits duration, duplicate endings may count twice, and dropped observations are not recovered.
Samples describe independent layers and cannot be summed into one call population or current fleet filesystem totals.
Historical resources remain valid without new capability fields or rebinding.
Automated backend qualification and separately authorized live acceptance remain distinct.

## Links
- [Work Item 184](https://github.com/jozala/omnigrex/issues/184)
- [Existing live boundary](0053-export-live-budget-observations-in-the-orchestrator.md)
- [Shared storage/tool policy](0062-sample-mounted-storage-without-fleet-accounting.md)
- [Runtime producer contract](../observability/application.md#live-runtime-producers)
