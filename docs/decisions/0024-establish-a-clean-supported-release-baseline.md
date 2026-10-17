---
status: accepted
date: 2026-10-16
decision-makers: [Mariusz]
---
# Establish a clean supported release baseline

## Context

The operator explicitly accepts discarding existing deployment state to establish a simpler supported implementation.
Guest paths and database structures are durable contracts, so silently changing them beneath retained Participants would violate their identity and recovery guarantees.
Future Pi image upgrades must continue to support qualified Session Continuation and History Replay from the new baseline.

## Decision

Introduce one fresh database schema baseline and support only `pi-rpc/v2`, using neutral Omnigrex guest storage paths.
Use `OMNIGREX_PI_IMAGE` and `OMNIGREX_PI_PLATFORM` for deployment selection without deprecated aliases.
Existing database histories and pre-baseline Session archives are incompatible and require an explicit fresh installation.
Do not convert, rebind, or implicitly delete deployment state during startup.

Remove unused compatibility plumbing, historical cutover tooling, and execution-readiness fallbacks that bypass the selected Runtime Profile.
Keep migration-history validation and forward-only migrations after this baseline.
Keep both supported Execution Backends, current recovery invariants, and backend-scoped source-to-candidate Pi upgrade qualification on isolated copies.

## Rationale

A clean schema and one supported guest contract eliminate transitional branches without weakening current ownership or fencing.
An explicit reset boundary avoids disguising a breaking storage change as an ordinary upgrade.
Preserving future upgrade qualification prevents this one-time cleanup from making routine data disposal a product requirement.

## Consequences

All deployment bundles, repository Agent Profiles, fixtures, and diagnostics must select the new contract consistently.
The new published release requires fresh database, runtime-state, workspace, tool, and native-backend storage.
Only qualification evidence generated for the clean baseline is accepted.
The operator guide describes the supported installation and future upgrades; historical implementation details remain in the delivery plans.

## Links

- [Implementation plan](../plans/2026-10-16-clean-runtime-baseline.md).
- [Exact Participant image ownership](0009-pin-participant-images-and-use-working-revision-tools.md).
- [Backend-scoped Pi qualification](0017-bind-pi-qualification-to-backend-contracts.md).
