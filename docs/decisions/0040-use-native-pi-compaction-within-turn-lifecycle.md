---
status: accepted
date: 2026-10-25
decision-makers: [Implementation agent]
---
# Use native Pi compaction within the existing Turn lifecycle

## Context
Long-running assignments need context reduction without replacing durable Agent Sessions or duplicating Pi's context-management logic.
The sanitization boundary removes the explicit overflow signal Pi needs, and retained Sessions can compact before RPC prompt admission.
Pi 1.4.0 supplies native compaction and pre-prompt cancellation support; older pinned images retain their behavior.

## Decision
Enable native compaction in controlled in-memory settings, inheriting Pi's thresholds and the selected model's capacity metadata.
Keep ordinary provider retries disabled.
Preserve only an explicit context/input-overflow classification as fixed text understood by Pi, not raw errors or ambiguous account token-limit failures.
Project compaction summaries and provider errors out of operational RPC events while retaining protected Session state and append-order replay.

Bound prompt admission by the existing Turn deadline because pre-prompt summary generation is legitimate execution, not an ordinary short RPC.
Continue to require `agent_settled`; compaction events do not settle a Turn.
An unresolved native compaction failure at settlement enters existing bounded provider-failure handling, and aborted compaction is not successful completion.
A later successful conversational request can supersede a failed pre-prompt compaction, consistent with native compact-and-continue behavior.
Retain cancellation evidence across Pi's synthetic preflight assistant error when cancellation was requested.

## Rationale
A custom summarizer, controller recovery loop, increased retry budget, or Session renewal would duplicate native behavior and risk bypassing existing admission and fencing.
Keeping the ordinary RPC timeout for prompt admission would interrupt legitimate retained-context compaction.
Safe event projection extends the existing owned subscription boundary rather than logging raw SDK events.

## Consequences
The feature requires newly qualified runtime images; existing Participants are not rebound.
No threshold configuration, capacity discovery, new step counter, or telemetry pipeline is introduced.
Standalone compaction does not establish combined step-budget qualification, which remains a separate obligation.

## Links
- [Work Item 129](https://github.com/jozala/omnigrex/issues/129)
- [Combined qualification 130](https://github.com/jozala/omnigrex/issues/130)
- [Owned settings and sanitization](0014-own-pi-resource-loading-with-a-pinned-runner.md)
- [Durable history](0015-reserve-exact-pi-sessions-and-replay-bounded-history.md)
- [RPC projection](0039-project-pi-control-events.md)
- [Operator guidance](../operators/context-compaction.md)
