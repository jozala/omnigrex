---
status: accepted
date: 2026-10-09
decision-makers: [Implementation agent]
---
# Own Pi resource loading with a pinned runner

## Context

[Decision 0006](0006-use-pi-through-an-agent-specific-adapter.md) requires an owned MCP extension and controlled runtime-resource loading.
Pi's CLI loads settings, performs migrations, and resolves project trust before constructing its runtime services.
Resource-disabling CLI flags qualified in the feasibility experiment exclude the exercised poisoned resources, but do not provide an explicit settings-storage boundary.
The production boundary must also keep launch credentials out of execution-backend environment metadata.

## Decision

Use an Omnigrex-owned JavaScript runner with the pinned Pi SDK and Pi's existing JSONL RPC implementation.
Construct settings in memory with project trust disabled, disable automatic extensions, MCP, skills, prompt templates, themes, and context files, and supply the owned MCP extension as an inline factory.
Compose current-deployment instructions in Go and supply the exact resulting text through the resource loader's system-prompt override.
Authorize checkout-local `.agents/skills/` discovery only through the Profile's explicit `skill` permission, require `read` permission for skill use, and reject symlink-expanded skill discovery.

Send one bounded bootstrap frame over the backend's process transport before ordinary RPC commands.
Render only the selected provider's literal API key into a private disposable Pi-native `auth.json` file.
Reject environment-reference inputs and encode accepted literal dollar characters so Pi's template reader cannot transform their value.
Keep MCP authority in process memory and reject MCP discovery that differs from the expected per-Turn capability set.
Apply the exact-endpoint fetch policy to POST, optional SSE, and reconnection requests, rejecting redirects before forwarding authority.
Normalize provider failure messages through an owned pre-persistence extension hook, retaining failure/retry classification and numeric usage while discarding untrusted error text, partial content, and provider diagnostic metadata.

## Rationale

The SDK runner reuses the qualified Pi protocol while making settings and resource ownership explicit.
CLI-only flag composition leaves additional startup behavior outside this boundary, while a private RPC reimplementation would duplicate Pi's prompt and Session mechanics.
Transport bootstrap works through both execution backends without adding secret-bearing environment variables or backend-specific file-transfer contracts.
Rejecting skill symlinks keeps authorization scoped to the prepared checkout instead of silently importing external resources.

## Consequences

The runner and owned extension are runtime resources whose exact package pins require qualification with retained images.
Local `bash` authority and repository code can still read credentials intentionally supplied to their guest; this does not change [decision 0007](0007-supply-provider-api-keys-through-disposable-files.md)'s trust boundary.
Durable Session binding, History Replay, and complete Agent Turn composition remain assigned to Steps 11-12.
The preparatory diagnostic packages and exercises this runner on both backends.

## Links

- [Step 10 implementation and evidence](../plans/2026-10-09-pi-configuration-and-mcp.md)
- [Pi and execution-backend plan](../plans/2026-10-05-pi-and-pluggable-execution-backends.md)
- [Current instruction ownership](0002-use-current-deployment-agent-instructions.md)
