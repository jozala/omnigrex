---
status: accepted
date: 2026-10-26
decision-makers: [Implementation agent]
---
# Separate Pi compaction and retry timestamps

## Context
Pi 1.4.0 excludes assistant messages whose timestamps are less than or equal to the latest compaction timestamp from subsequent compaction checks.
A fast native overflow retry can begin in the same millisecond as compaction, causing its new overflow error to be treated as stale and suppressing the exhausted-recovery event.
The controlled provider qualification reproduces this boundary without paid requests.

## Decision
Use an isolated, deployment-owned compatibility extension at Pi's awaited `session_compact` hook to separate successful compaction from the next request's timestamp.
Read the latest compaction from the public branch view because the SDK event can reference an older entry when summary text repeats.
Bound the hook to 32 asynchronous one-millisecond timer waits and retain native provider-failure handling if an abnormal clock does not advance.
Keep Pi's native summaries, compact-and-retry allowance, error classification, and settlement authority unchanged.
Preserve an explicitly requested cancellation confirmed by the native settlement receipt, even when compaction completed successfully before the abort.
Do not modify retained timestamps, monkey-patch SDK internals, add controller retries, or slow only the test provider.
Include the extension in new Runtime Profiles' owned-resource inventories while accepting historical inventories without it.

## Rationale
The public hook runs after the compaction entry is appended and before native continuation, so a scheduling boundary can avoid the millisecond collision without changing recovery policy.
A fixture-only delay would hide a real runtime defect, while a fork or SDK-internal override would expand the maintenance and qualification surface.
An unbounded wall-clock wait could stall continuation after a clock rollback, so the compatibility hook is deliberately best-effort under abnormal clock behavior.

## Consequences
New images require ordinary both-backend qualification, and existing Participants retain their pinned images and behavior.
Keep deterministic regression coverage of the collision and remove the compatibility extension when a qualified Pi release uses an unambiguous ordering boundary instead of timestamps.
This is a timing workaround, not a replacement context-management implementation or a guarantee against arbitrary host clock changes.

## Links
- [Native compaction policy](0040-use-native-pi-compaction-within-turn-lifecycle.md)
- [Owned runtime resources](0014-own-pi-resource-loading-with-a-pinned-runner.md)
- [Failing CI qualification](https://github.com/jozala/omnigrex/actions/runs/40247605116/job/121332396278)
