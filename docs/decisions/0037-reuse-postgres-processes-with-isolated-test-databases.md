---
status: accepted
date: 2026-10-21
decision-makers: [OpenCode implementation agent]
---
# Reuse PostgreSQL test processes while isolating databases

## Context

The instrumented PR CI store suite created 340 PostgreSQL containers and spent 9m 32s of measured work on container lifecycle, including 7m 36s waiting for readiness.
The user requested process reuse without weakening database isolation or migration coverage.
Tests exercise concurrent connections, committed transactions, migration history corruption, failed DDL, and session advisory locks, so sharing tables or wrapping each test in a rollback-only transaction would change the behavior under test.

## Decision

Lazily start one hardened, pinned PostgreSQL container per ordinary store integration test-binary invocation.
Give every fixture a uniquely owned empty database created from `template0`, with its own application pools and the normal migration path.
Do not clone an application schema or cache migration results.
Drop each fixture database during test cleanup, terminating its leaked connections without affecting sibling databases.
Remove the shared container in integration-only `TestMain`, including after test failure.
Keep dedicated fresh-process startup/password and concurrent-migration qualification, fresh/existing migration probes, and the independent migration-engine fixtures.

Retain fsync, synchronous commits, full-page writes, and existing container hardening.
Bound fixture WAL/checkpoint configuration rather than disabling durability or growing the runtime resource contract.
Record container and per-database timings separately, and identify store-opening samples by database as well as server address.

## Consequences

Ordinary database tests avoid repeated process initialization while preserving fresh schemas, committed state, and multiple independent connections.
The fixture manager owns the shared process lifecycle; application pools remain owned by each test.
Server-global mutations or server restart/crash scenarios must use an explicitly isolated process rather than the shared helper.
Tests that do not request a database, including externally provisioned Pi qualification, do not start a local PostgreSQL container.
This decision does not introduce general test parallelism or persistent build caching.

## Links

- [PR timing diagnostics and fixture reuse](https://github.com/jozala/omnigrex/pull/107).
- [Measured PR CI run](https://github.com/jozala/omnigrex/actions/runs/39516915730).
- [Contributor testing guide](../contributors/testing.md#investigate-test-duration).
