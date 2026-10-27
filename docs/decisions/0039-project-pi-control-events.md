---
status: accepted
date: 2026-10-25
decision-makers: [Implementation agent]
---
# Project Pi events at the owned RPC subscription

## Context
Pi 1.0.4 serializes `agent_end.messages` even though Omnigrex uses `agent_settled` and assistant stop evidence, not that transcript, to settle an Agent Turn.
Long Agent Turns can therefore exceed the bounded control-channel frame size after a successful terminal MCP operation.
The pinned RPC runner has no output-transform option.

## Decision
Give the stock RPC runner an owned view of the public runtime and Session subscription APIs.
Project transcript-bearing events before the RPC listener serializes them, without changing the real Session or its other subscribers.
Keep `agent_end.type` and `willRetry`, assistant role/stop/error evidence, retry events, and the authoritative `agent_settled` receipt.
Project redundant message, tool, entry, queue, and compaction bodies to control metadata as documented in the runtime qualification guide.
Keep Pi's command handling, response correlation, agent backpressure subscription, cancellation, rebinds, and disposal.
Keep the existing controller frame bound and fail-closed validation for remaining required or unknown records.

## Rationale
A subscription view uses the public SDK boundary and avoids patching stdout, modifying installed packages, or forking the RPC implementation.
Raising the frame limit only moves the failure threshold; ignoring oversized frames can discard required evidence.
Durable conversation history and bounded History Replay remain separate from operational RPC activity, consistent with decisions 0006 and 0015.

## Consequences
The view is qualified against pinned Pi 1.0.4 rather than claiming general compatibility with every Pi event consumer.
Pi upgrades must requalify subscription access, JSON event conversion, backpressure, cleanup, and any new transcript-bearing event types.
Required scalar fields and unknown events are not truncated or silently dropped; an oversized required record still fails closed.
No settlement or terminal-intent reconciliation rule changes.

## Links
- [Work Item 117](https://github.com/jozala/omnigrex/issues/117)
- [Adapter ownership](0006-use-pi-through-an-agent-specific-adapter.md)
- [Bounded History Replay](0015-reserve-exact-pi-sessions-and-replay-bounded-history.md)
- [Qualification and event audit](../contributors/runtime-qualification.md#owned-rpc-event-projection)
