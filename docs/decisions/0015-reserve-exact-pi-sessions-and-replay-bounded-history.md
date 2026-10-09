---
status: accepted
date: 2026-10-09
decision-makers: [OpenCode implementation agent]
---
# Reserve exact Pi Sessions and replay bounded append-order history

## Context

Pi normally delays file creation until a conversation message and discovers exact IDs through a best-effort directory scan.
Omnigrex must recover interrupted preparation without guessing a conversation, silently recreating activated state, or depending on the previous workspace.
Pi's native `get_entries` returns the entire transcript in one JSONL response, which can exceed the existing bounded transport contract.

## Decision

Reserve the durable Agent Session UUID as its immutable Pi runtime identity under the Agent Turn fence before installing runtime inputs.
The owned runner atomically publishes a synced version-3 header at the exact Session-local file before reporting readiness.
Validate that file before Pi opens it, reject missing activated state and malformed or mismatched files, and perform no implicit format migration.

Keep ACP identity and capabilities separate during preparation-stage compatibility; the schema addition does not convert existing conversations.
The runtime-neutral coordinator revalidates ownership before activation and every prompt and checks the live connection's exact prepared identity.

Multiplex an owned, read-only history operation with Pi's unchanged RPC reader.
Replay a stable append-order snapshot in pages of at most 128 metadata entries, including abandoned branches, compactions, and context edits.
Keep transcript bodies inside the adapter and advance a serializable Session-scoped cursor only after the sink acknowledges each Agent Event.
Human prompting requires complete successful replay under its exclusive admission lease.
An unknown human prompt outcome preserves that lease token until the execution owner establishes a stopped boundary.

## Rationale

Latest-file or scan-based selection introduces ambiguity and hides protected-state loss.
Pi's normal lazy file creation does not establish pre-prompt persistence, and its permissive parser can ignore damaged history.
Using current model context for replay loses historical entries after branching or compaction.
Increasing the transport frame limit to fit arbitrary transcripts weakens bounded behavior; metadata pages preserve that bound without modifying Pi's model or prompt protocol.

## Consequences

Retained state uses the explicit `pi-session/v3` contract and remains separate from disposable provider and MCP credentials.
Pi-to-Pi format changes require later image/state qualification rather than migration during ordinary reopening.
The diagnostic's in-memory admission ledger demonstrates sequencing; PostgreSQL integration tests supply durable fencing evidence.
Full Agent Turn composition remains Step 12, and the transitional ACP coordinator is removed with OpenCode in Step 17.

## Links

- [Step 11 implementation and evidence](../plans/2026-10-09-durable-pi-sessions.md).
- [Agent adapter ownership](0006-use-pi-through-an-agent-specific-adapter.md).
- [Retained-state ownership](0011-bind-retained-state-and-validate-offline-session-archives.md).
- [Owned runner](0014-own-pi-resource-loading-with-a-pinned-runner.md).
