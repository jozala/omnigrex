---
status: accepted
date: 2026-10-07
decision-makers: [Mariusz]
---
# Supply provider API keys through disposable files

## Context

Pi requires provider authentication inside each Runtime Process.
Microsandbox offers a possible host-side secret mediation mechanism.
Mariusz selected ephemeral real API keys on both initial backends and deferred credential mediation.
Subscription/OAuth requests are distinct from operating durable, concurrent token refresh across disposable processes.

## Decision

Keep the deployment secret file under operator control and render Pi-native API-key configuration into each Runtime Process's disposable agent directory.
Resolve the selected provider's required credentials without an Omnigrex model catalog or provider fallback.
Do not deliver unrelated provider entries merely because they share the deployment bundle.
Keep provider credentials separate from Retained Runtime State and per-Turn MCP authority.

Do not serialize credential inputs into database records, labels, image layers, instruction bundles, ordinary diagnostic output, or state-backup manifests.
Delete the disposable credential material with its execution lifecycle.
Leave provider mediation, OAuth refresh persistence, and refresh concurrency to separate work.

## Rationale

This supports the qualified API-key path on both backends without making their initial delivery depend on a new credential broker.
Microsandbox-native substitution may later implement stronger guarantees behind the boundary, but is not assumed equivalent to a general provider proxy.

## Consequences

This supersedes ADR 0010's credential-delivery mechanism.
The guest can read credentials intentionally supplied to it; stronger host isolation does not make those credentials inaccessible to repository code.
State audits establish the normal runtime/configuration path, not prevention of deliberate guest exfiltration or copying into session content.
Operational procedures remain in the operator guide.

## Links

- [Contract design](../plans/2026-10-07-runtime-boundary-contracts.md)
- [Superseded ADR 0010](../plans/runtime-history/adr-0010.md)
- [Pi credential-path evidence](../experiments/pi.md)
- [Operator secret handling](../operators/secrets.md)
