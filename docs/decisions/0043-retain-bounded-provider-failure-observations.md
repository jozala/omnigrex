---
status: accepted
date: 2026-10-25
decision-makers: [Implementation agent]
---
# Retain bounded provider-failure observations, not raw errors

## Context
The failure boundary deliberately removes provider errors before Pi Session persistence and RPC delivery.
Broad subscription categories alone cannot distinguish credential resolution, an HTTP response, a failed stream, or a request that never reached the provider.
Request IDs and exception properties are untrusted and can contain credentials even when syntactically plausible.

## Decision
Retain a closed diagnostic vocabulary, bounded status/timing fields, and locally generated request-attempt IDs through the existing pre-persistence sanitation boundary.
Capture subscription observations within one asynchronous request context, including controller authentication before provider streaming, and bind them to the actual terminal assistant message with a weak reference.
Native compaction and branch-summary requests bypass conversational message events, so also deliver each failed request’s closed observation through a Session-scoped owned control subscription, without adding Session messages or replacing Pi’s lifecycle events.
Revalidate the schema in the Go adapter and emit ordinary correlated controller events for each failed request and terminal settlement.
Retain only a SHA-256 fingerprint of a bounded `x-request-id` header, never its original value.
Keep unknown evidence explicit and preserve the broad subscription failure in returned diagnostics without changing retry or settlement policy.

## Rationale
Logging raw errors or allowing arbitrary syntactically valid codes and IDs would reopen the secret-bearing boundary.
A header fingerprint permits correlation with independently obtained provider evidence without exposing an arbitrary header value; it is not the original provider request ID.
Keeping only the most recent global error can associate evidence with the wrong request or concurrent execution.
The outer public streaming wrapper covers credential resolution that happens before the native provider's stream function is invoked; the pinned SDK's lazy stream preserves its error settlement semantics.

## Consequences
Unknown provider codes retain known stage and HTTP status but not their content.
Other providers retain unavailable observations rather than fabricated provider-specific details.
Operators can diagnose the request path after cleanup, but may still need separately authorized provider support evidence to resolve an unknown cause.
Pi upgrades must requalify message identity, asynchronous context, persistence, event projection, and both backend gates.
No paid probe, new metric label, credential routing change, or raw debug logging is introduced.

## Links
- [Work Item 127](https://github.com/jozala/omnigrex/issues/127)
- [Controller-owned credentials](0032-own-chatgpt-subscription-credentials-in-the-controller.md)
- [RPC event projection](0039-project-pi-control-events.md)
